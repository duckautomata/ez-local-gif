// Command ezlg is the ez-local-gif server.
//
//	ezlg [serve]        run the HTTP server (default)
//	ezlg testkit ...    emit the Discord test-variant matrix (see internal/testkit)
//	ezlg version
//
// Configuration is by environment variables (all optional):
//
//	EZLG_ADDR            listen address            (default ":8080")
//	EZLG_DATA            data root                 (default "/data")
//	EZLG_SCRATCH         scratch (tmpfs) root      (default "/dev/shm/ezl", falls back to $TMPDIR/ezl)
//	EZLG_TTL_HOURS       delete blobs/results older than this (default 24; 0 = never)
//	EZLG_MAX_BYTES       cap on total /data size in bytes (default 20 GiB; 0 = none)
//	EZLG_MAX_UPLOAD_MB   max upload size            (default 2048); also caps what one
//	                     POST /api/sources/from-input pick may ingest from /input
//	                     (jobs.Options.MaxUploadBytes) — an over-limit pick is a 413
//	                     like an upload of the same bytes
//	EZLG_CONCURRENCY     concurrent renders         (default max(1, NumCPU/2))
//	EZLG_MAX_MASTER_BYTES cap on one render's RGBA frame master in bytes (default 2 GiB =
//	                     2147483648; <= 0 keeps the default; above jobs.MaxMasterBytesCeiling,
//	                     512 PiB, it is logged and clamped). A recipe whose master (at the
//	                     output size) would exceed it is refused up-front
//	                     (jobs.Options.MaxMasterBytes) — the error names this variable, so it
//	                     must be a real knob. It is the only frame-master cap (the graph never
//	                     refuses a plan for its frame count) and so also the sole bound on the
//	                     RAM ffmpeg's reverse filter buffers for reversed/bounced previews and
//	                     static (png/jpeg) reversed renders: raised past host RAM, those become
//	                     OOM kills instead of refusals. Forward stills and proxies are never
//	                     gated by it; GET /api/capabilities publishes it as maxMasterBytes so
//	                     the SPA shows the estimate before Render.
//	EZLG_OUTPUT          "Save to /output" directory (default "/output"); checked once at
//	                     startup — when it is missing or not writable the save feature is
//	                     off (capabilities features.outputSave false, the endpoint answers 503)
//	EZLG_INPUT           read-only "pick from /input" directory (default "/input"); checked
//	                     once at startup like EZLG_OUTPUT (features.inputPick); the listing
//	                     itself is read per request
//	EZLG_FFMPEG etc.     override tool paths (see ffrun.LookupTools; EZLG_FC_LIST for the
//	                     fontconfig fc-list behind GET /api/fonts)
//
// On SIGINT/SIGTERM the server stops accepting connections, lets in-flight
// requests (uploads, stills, downloads) finish, cancels running renders
// (killing their ffmpeg/gifsicle and removing their scratch dirs) and ends
// open progress streams — for up to drainTimeout — before the process exits.
// A second signal during the drain kills the process immediately.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/ffrun"
	"github.com/duckautomata/ez-local-gif/internal/jobs"
	"github.com/duckautomata/ez-local-gif/internal/server"
	"github.com/duckautomata/ez-local-gif/internal/store"
	"github.com/duckautomata/ez-local-gif/web"
)

// Version is set at build time via -ldflags "-X main.Version=…".
var Version = "dev"

// drainTimeout bounds the graceful shutdown (HTTP drain + render
// cancellation). It sits under Docker's default 10 s stop grace period
// (after which the container is SIGKILLed) so a `docker stop` normally ends
// with a clean exit rather than a kill.
const drainTimeout = 8 * time.Second

// sweepInterval is the cadence of the /data sweeper.
const sweepInterval = 10 * time.Minute

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			log.Fatal(err)
		}
	case "version":
		fmt.Println(Version)
	case "testkit":
		if err := runTestkit(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
		log.Printf("ignoring invalid %s=%q", k, v)
	}
	return def
}

// serveConfig is the environment-derived configuration of `ezlg serve`.
type serveConfig struct {
	addr      string
	dataRoot  string
	scratch   string
	ttl       time.Duration
	maxBytes  int64
	maxUpload int64
	conc      int
	// maxMaster is jobs.Options.MaxMasterBytes (EZLG_MAX_MASTER_BYTES); <= 0
	// lets the manager apply jobs.DefaultMaxMasterBytes.
	maxMaster int64
	// outputDir/inputDir are jobs.Options.OutputDir/InputDir (EZLG_OUTPUT /
	// EZLG_INPUT, Phase 4); the manager disables the matching feature when
	// the directory is not usable at startup.
	outputDir string
	inputDir  string
	drain     time.Duration
}

func serveConfigFromEnv() serveConfig {
	return serveConfig{
		addr:      envStr("EZLG_ADDR", ":8080"),
		dataRoot:  envStr("EZLG_DATA", "/data"),
		scratch:   envStr("EZLG_SCRATCH", "/dev/shm/ezl"),
		ttl:       time.Duration(envInt("EZLG_TTL_HOURS", 24)) * time.Hour,
		maxBytes:  envInt("EZLG_MAX_BYTES", 20<<30),
		maxUpload: envInt("EZLG_MAX_UPLOAD_MB", 2048) << 20,
		conc:      int(envInt("EZLG_CONCURRENCY", int64(max(1, runtime.NumCPU()/2)))),
		maxMaster: envInt("EZLG_MAX_MASTER_BYTES", jobs.DefaultMaxMasterBytes),
		outputDir: envStr("EZLG_OUTPUT", "/output"),
		inputDir:  envStr("EZLG_INPUT", "/input"),
		drain:     drainTimeout,
	}
}

// serve runs the server until SIGINT/SIGTERM, then drains and returns.
func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// The first signal starts the drain. Restoring default signal
		// handling right away means a second Ctrl-C / SIGTERM (or Docker's
		// SIGKILL) ends the process at once instead of being swallowed.
		<-ctx.Done()
		stop()
	}()
	return runServer(ctx, serveConfigFromEnv(), nil)
}

// runServer wires the store, job manager and HTTP layer for cfg, serves on
// ln (or listens on cfg.addr when ln is nil) until ctx is cancelled, then
// shuts down gracefully — for at most cfg.drain — and returns. It returns
// nil after a clean or timed-out drain and an error only when serving
// itself failed (e.g. the address is in use).
func runServer(ctx context.Context, cfg serveConfig, ln net.Listener) error {
	tools := ffrun.LookupTools()
	if tools.FFmpeg == "" || tools.FFprobe == "" {
		return errors.New("ffmpeg and ffprobe are required (set EZLG_FFMPEG / EZLG_FFPROBE or add them to PATH)")
	}
	if tools.Gifsicle == "" {
		log.Printf("warning: gifsicle not found; GIF optimisation disabled")
	}
	if tools.FcList == "" {
		log.Printf("warning: fc-list not found; the font list (GET /api/fonts) will be empty — text overlays still resolve bundled families by name")
	}

	st, err := store.New(cfg.dataRoot, cfg.scratch)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	// One resolved upload cap for both layers: the HTTP upload path
	// (server.Config.MaxUploadBytes) and the /input picker's ingest
	// (jobs.Options.MaxUploadBytes) must agree, or a pick could ingest what
	// an upload of the same bytes would refuse with 413. NewServer would
	// substitute the default itself for a <= 0 value; resolving it here
	// keeps the manager on the identical number.
	maxUpload := cfg.maxUpload
	if maxUpload <= 0 {
		maxUpload = server.DefaultMaxUploadBytes
	}
	jm := jobs.NewManager(st, tools, jobs.Options{
		Concurrency:    cfg.conc,
		MaxMasterBytes: cfg.maxMaster,
		OutputDir:      cfg.outputDir,
		InputDir:       cfg.inputDir,
		MaxUploadBytes: maxUpload,
	})
	log.Printf("phase 4 file exchange: save to %s = %v (EZLG_OUTPUT), pick from %s = %v (EZLG_INPUT)",
		cfg.outputDir, jm.OutputSaveEnabled(), cfg.inputDir, jm.InputPickEnabled())

	h := server.NewServer(server.Config{MaxUploadBytes: maxUpload, Version: Version}, st, jm, tools, web.Dist())
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: SSE streams and large downloads are long-lived.
	}

	// Sweeper: runs under ctx, so it stops when the drain begins.
	if cfg.ttl > 0 || cfg.maxBytes > 0 {
		go func() {
			t := time.NewTicker(sweepInterval)
			defer t.Stop()
			for {
				if err := st.Sweep(ctx, cfg.ttl, cfg.maxBytes); err != nil && !errors.Is(err, store.ErrNotImplemented) && ctx.Err() == nil {
					log.Printf("sweep: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	if ln == nil {
		ln, err = net.Listen("tcp", cfg.addr)
		if err != nil {
			return err
		}
	}
	log.Printf("ez-local-gif %s listening on %s (data=%s scratch=%s ffmpeg=%s concurrency=%d maxMasterBytes=%d)",
		Version, ln.Addr(), cfg.dataRoot, cfg.scratch, tools.FFmpeg, jm.Concurrency(), jm.MaxMasterBytes())

	// Serve on its own goroutine: Serve returns ErrServerClosed the instant
	// Shutdown closes the listener, and the process must not exit then —
	// it must wait for the drain below to complete.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Printf("shutting down: draining for up to %s", cfg.drain)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.drain)
	defer cancel()

	// Both halves start at once and share the deadline: h.Shutdown cancels
	// every running render and preview pre-warm and ends open SSE streams
	// (after forwarding their terminal event) while srv.Shutdown stops
	// accepting connections and lets in-flight uploads, stills and
	// downloads finish. Neither should wait on the other to begin.
	bgDone := make(chan error, 1)
	go func() { bgDone <- h.Shutdown(shutdownCtx) }()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Out of time: whatever is still connected gets cut off now rather
		// than at process exit.
		log.Printf("shutdown: HTTP drain incomplete: %v", err)
		_ = srv.Close()
	}
	if err := <-bgDone; err != nil {
		log.Printf("shutdown: %v", err)
	}
	// Serve has returned once Shutdown closed the listener; report only a
	// genuine accept failure that raced with the signal.
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("serve: %v", err)
	}
	return nil
}

// runTestkit points at the Phase 1 test-variant generator, which is a shell
// script that runs inside the container (scripts/discord-testkit.sh). A Go
// implementation that reuses internal/enc lands with Phase 2.
func runTestkit(args []string) error {
	fmt.Println("Phase 1: the Discord test-variant generator is a bash script baked into the image. Run it with:")
	fmt.Println("  docker compose run --rm --entrypoint bash app /usr/local/share/ezlg/discord-testkit.sh /output/testkit")
	fmt.Println("(compose.yaml mounts ./output:/output; see docs/DEVELOPMENT.md 'Discord acceptance test')")
	return nil
}

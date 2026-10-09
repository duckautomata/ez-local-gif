package matte

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ping5cJSON is a Phase 5c ping: one process offering two devices, the
// per-model device map beside the mirrored default-device fields, and a
// tracker model.
const ping5cJSON = `{"protocol":1,"version":"2026.10.2","instance":"d4e5f6","processingVersion":"1",
 "device":"cuda","reason":"","devices":["cuda","cpu"],"defaultDevice":"cuda",
 "defaultModel":"birefnet-lite","defaultModels":{"cuda":"birefnet-lite","cpu":"isnet-anime"},
 "gpu":{"name":"NVIDIA GeForce RTX 5080","totalGiB":16,"freeGiB":12.4},
 "models":{
  "isnet-anime":{"state":"ready","reason":"","percent":100,"weights":"f15622d8","graphDigest":"0123",
   "precision":"fp16","sizes":[1024,512],"defaultSize":1024,"msPerFrame":{"1024":18,"512":9},
   "licence":"Apache-2.0","lastError":"","label":"Anime (fast)","kind":"segmenter","resident":false,
   "devices":{
    "cuda":{"state":"ready","reason":"","percent":100,"precision":"fp16","size":1024,"sizes":[1024,512],"msPerFrame":{"1024":18,"512":9},"resident":false},
    "cpu":{"state":"ready","reason":"","percent":100,"precision":"fp32","size":512,"sizes":[512,1024],"msPerFrame":{"512":136},"resident":true}}},
  "birefnet-lite":{"state":"ready","reason":"","percent":100,"weights":"ee11ee11","graphDigest":"4567",
   "precision":"fp32","sizes":[1024],"defaultSize":1024,"msPerFrame":{"1024":170.5},
   "licence":"MIT","lastError":"","label":"General (precise)","kind":"segmenter","resident":true,
   "devices":{
    "cuda":{"state":"ready","reason":"","percent":100,"precision":"fp32","size":1024,"sizes":[1024],"msPerFrame":{"1024":170.5},"resident":true},
    "cpu":{"state":"unavailable","reason":"cpu: 14 GiB of RAM needed, 9 GiB available","percent":0,"precision":"fp32","size":1024,"sizes":[1024],"msPerFrame":{},"resident":false}}},
  "sam2-tiny":{"state":"ready","reason":"","percent":100,"weights":"7402e0d8","graphDigest":"",
   "precision":"bf16","sizes":[],"defaultSize":0,"msPerFrame":{"1024":30},
   "licence":"Apache-2.0","lastError":"","label":"Guided (click to select)","kind":"tracker","resident":false,
   "devices":{
    "cuda":{"state":"ready","reason":"","percent":100,"precision":"bf16","size":0,"sizes":[],"msPerFrame":{"1024":30},"resident":false}}}},
 "busy":0}`

func parsePing(t *testing.T, js string) *Ping {
	t.Helper()
	var p Ping
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func TestPing5cDevices(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, ping5cJSON) })
	p, err := c.Ping(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Devices, []string{"cuda", "cpu"}) || p.DefaultDevice != DeviceCUDA || p.Device != DeviceCUDA {
		t.Errorf("devices: %v default %q device %q", p.Devices, p.DefaultDevice, p.Device)
	}
	if !reflect.DeepEqual(p.DefaultModels, map[string]string{"cuda": "birefnet-lite", "cpu": "isnet-anime"}) || p.DefaultModel != "birefnet-lite" {
		t.Errorf("default models: %v / %q", p.DefaultModels, p.DefaultModel)
	}
	isnet, _ := p.Model("isnet-anime")
	if isnet.Kind != KindSegmenter || isnet.IsTracker() || isnet.Resident {
		t.Errorf("isnet kind/resident: %+v", isnet)
	}
	wantCPU := DeviceState{State: StateReady, Percent: 100, Precision: "fp32", Size: 512, Sizes: []int{512, 1024},
		MsPerFrame: map[string]float64{"512": 136}, Resident: true}
	if got, ok := isnet.On(DeviceCPU); !ok || !reflect.DeepEqual(got, wantCPU) {
		t.Errorf("isnet on cpu: ok %v\n got %+v\nwant %+v", ok, got, wantCPU)
	}
	// The top-level fields mirror the default device: On("") and Default() say the same as On("cuda").
	cuda, ok := isnet.On(DeviceCUDA)
	if !ok {
		t.Fatal("isnet not offered on cuda")
	}
	if def, ok := isnet.On(""); !ok || !reflect.DeepEqual(def, cuda) || !reflect.DeepEqual(isnet.Default(), cuda) {
		t.Errorf("default-device mirror:\n  On(\"\") %+v\n  cuda    %+v", def, cuda)
	}
	lite, _ := p.Model("birefnet-lite")
	if !lite.Resident {
		t.Error("lite resident flag not read")
	}
	if d, ok := lite.On(DeviceCPU); !ok || d.State != StateUnavailable || !strings.Contains(d.Reason, "14 GiB") {
		t.Errorf("lite on cpu: %v %+v", ok, d)
	}
	sam, ok := p.Model("sam2-tiny")
	if !ok || !sam.IsTracker() || sam.Kind != KindTracker || sam.Label != "Guided (click to select)" {
		t.Errorf("tracker: %v %+v", ok, sam)
	}
	if _, ok := sam.On(DeviceCPU); ok {
		t.Error("tracker reported on cpu, which its device map does not list")
	}
	if d, ok := sam.On(DeviceCUDA); !ok || d.Precision != "bf16" || d.MsPerFrameAt(1024) != 30 || d.MsPerFrameAt(0) != 0 {
		t.Errorf("tracker on cuda: %v %+v", ok, d)
	}
	// Device helpers.
	if p.EffectiveDevice("") != DeviceCUDA || p.EffectiveDevice(DeviceCPU) != DeviceCPU || p.EffectiveDevice("tpu") != "tpu" {
		t.Error("EffectiveDevice")
	}
	if !p.Offers("") || !p.Offers(DeviceCUDA) || !p.Offers(DeviceCPU) || p.Offers("tpu") || p.Offers(DeviceUnavailable) {
		t.Error("Offers")
	}
	if p.DefaultModelFor("") != "birefnet-lite" || p.DefaultModelFor(DeviceCUDA) != "birefnet-lite" ||
		p.DefaultModelFor(DeviceCPU) != "isnet-anime" || p.DefaultModelFor("tpu") != "birefnet-lite" {
		t.Error("DefaultModelFor")
	}
	// DeviceState helpers.
	d, _ := isnet.On(DeviceCPU)
	if d.EffectiveSize(0) != 512 || d.EffectiveSize(1024) != 1024 || !d.HasSize(1024) || d.HasSize(640) || d.HasSize(0) {
		t.Error("DeviceState size helpers")
	}
	if d.MsPerFrameAt(0) != 136 || d.MsPerFrameAt(512) != 136 || d.MsPerFrameAt(1024) != 0 {
		t.Error("DeviceState.MsPerFrameAt")
	}
	var zero DeviceState
	if zero.MsPerFrameAt(0) != 0 || zero.HasSize(1024) || zero.EffectiveSize(0) != 0 {
		t.Error("zero DeviceState")
	}
}

func TestPingPre5cCompat(t *testing.T) {
	// A 5b sidecar: no devices list, no per-model map — one device, its Device.
	p := parsePing(t, pingJSON)
	if p.Devices != nil || p.DefaultDevice != "" || p.DefaultModels != nil {
		t.Errorf("pre-5c ping grew fields: %+v", p)
	}
	if p.EffectiveDevice("") != DeviceCUDA {
		t.Errorf("EffectiveDevice(\"\") = %q, want the ping's Device", p.EffectiveDevice(""))
	}
	if !p.Offers("") || !p.Offers(DeviceCUDA) || p.Offers(DeviceCPU) {
		t.Error("a pre-5c sidecar offers exactly its device")
	}
	if p.DefaultModelFor("") != "isnet-anime" || p.DefaultModelFor(DeviceCPU) != "isnet-anime" {
		t.Error("DefaultModelFor falls back to defaultModel")
	}
	isnet, _ := p.Model("isnet-anime")
	if isnet.Kind != "" || isnet.IsTracker() || isnet.Devices != nil {
		t.Errorf("pre-5c model: %+v", isnet)
	}
	want := DeviceState{State: StateReady, Percent: 100, Precision: "fp16", Size: 1024, Sizes: []int{1024, 512},
		MsPerFrame: map[string]float64{"1024": 18, "512": 9}}
	for _, dev := range []string{"", DeviceCUDA, DeviceCPU} {
		if got, ok := isnet.On(dev); !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("On(%q) without a device map: ok %v %+v", dev, ok, got)
		}
	}
	// An unavailable device offers nothing.
	u := parsePing(t, `{"protocol":1,"device":"unavailable","reason":"CUDA EP not available","models":{}}`)
	if u.Offers("") || u.Offers(DeviceCUDA) || u.EffectiveDevice("") != DeviceUnavailable {
		t.Error("an unavailable device is offered")
	}
	var nilPing *Ping
	if nilPing.Offers("") || nilPing.DefaultModelFor("") != "" || nilPing.EffectiveDevice("") != "" {
		t.Error("nil ping helpers")
	}
}

func TestDeviceQueryParams(t *testing.T) {
	var gotPath, gotQuery string
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/matte" {
			writeRecords(w, [][]byte{fakePNG(1)}, true)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	ctx := context.Background()
	each := func([]byte) error { return nil }
	// Matte: device= only when set; the pre-5c query is byte-identical otherwise.
	if err := c.Matte(ctx, "isnet-anime", DeviceCPU, 2, 1, bytes.NewReader(make([]byte, 12)), each); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/matte" || gotQuery != "device=cpu&frames=1&model=isnet-anime&size=2" {
		t.Errorf("matte on cpu: %s?%s", gotPath, gotQuery)
	}
	if err := c.Matte(ctx, "isnet-anime", "", 2, 1, bytes.NewReader(make([]byte, 12)), each); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "frames=1&model=isnet-anime&size=2" {
		t.Errorf("matte on the default device: ?%s", gotQuery)
	}
	// Warm.
	if err := c.Warm(ctx, "birefnet-lite", DeviceCUDA); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/warm" || gotQuery != "device=cuda&model=birefnet-lite" {
		t.Errorf("warm: %s?%s", gotPath, gotQuery)
	}
	// Unload filters: model, device, both, neither.
	cases := []struct{ model, device, want string }{
		{"", "", ""},
		{"isnet-anime", "", "model=isnet-anime"},
		{"", DeviceCUDA, "device=cuda"},
		{"isnet-anime", DeviceCPU, "device=cpu&model=isnet-anime"},
	}
	for _, tc := range cases {
		if err := c.Unload(ctx, tc.model, tc.device); err != nil {
			t.Fatal(err)
		}
		if gotPath != "/v1/unload" || gotQuery != tc.want {
			t.Errorf("unload(%q, %q): %s?%s, want ?%s", tc.model, tc.device, gotPath, gotQuery, tc.want)
		}
	}
	// The sidecar's refusal of an unoffered device is an ordinary 404 StatusError.
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"device cpu not offered"}`)
	})
	err := c.Matte(ctx, "isnet-anime", DeviceCPU, 2, 1, bytes.NewReader(make([]byte, 12)), each)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 || se.Message != "device cpu not offered" {
		t.Errorf("unoffered device: %v", err)
	}
}

// boxPrompts is a two-frame prompt set: a box plus a negative click on
// frame 0 and a positive click on frame 12.
func boxPrompts() TrackPrompts {
	return TrackPrompts{Obj: 1, Prompts: []FramePrompt{
		{Frame: 0, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}, Points: [][3]float64{{0.7, 0.4, 0}}},
		{Frame: 12, Points: [][3]float64{{0.5, 0.5, 1}}},
	}}
}

const boxPromptsHeader = `{"obj":1,"prompts":[{"frame":0,"points":[[0.7,0.4,0]],"box":[0.1,0.2,0.6,0.9]},{"frame":12,"points":[[0.5,0.5,1]],"box":null}]}`
const boxPromptsCanonical = "obj=1|f=0:box=0.1000,0.2000,0.6000,0.9000:pts=0.7000,0.4000,0|f=12:box=-:pts=0.5000,0.5000,1"

func TestTrackRecords(t *testing.T) {
	const w, h, frames = 3, 2, 2
	body := make([]byte, TrackBodyLength(frames, w, h))
	for i := range body {
		body[i] = byte(i * 5)
	}
	if len(body) != 36 {
		t.Fatalf("TrackBodyLength(2, 3, 2) = %d", len(body))
	}
	want := [][]byte{fakePNG(1), fakePNG(2)}
	var gotMethod, gotPath, gotQuery, gotType, gotPrompts string
	var gotLen int64
	var gotBody []byte
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotType, gotLen, gotPrompts = r.Header.Get("Content-Type"), r.ContentLength, r.Header.Get(PromptsHeader)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ezlg-mattes")
		writeRecords(w, want, true)
	})
	var got [][]byte
	err := c.Track(context.Background(), "sam2-tiny", DeviceCUDA, w, h, frames, boxPrompts(), bytes.NewReader(body), func(png []byte) error {
		got = append(got, png)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/track" || gotQuery != "device=cuda&frames=2&h=2&model=sam2-tiny&w=3" {
		t.Errorf("request %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if gotType != ContentType || gotLen != 36 || !bytes.Equal(gotBody, body) {
		t.Errorf("body: type %q len %d equal %v", gotType, gotLen, bytes.Equal(gotBody, body))
	}
	if gotPrompts != boxPromptsHeader {
		t.Errorf("%s header:\n got %s\nwant %s", PromptsHeader, gotPrompts, boxPromptsHeader)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records:\n got %q\nwant %q", got, want)
	}
	// Default device: no device= on the wire; the prompts' click order does not reach the sidecar (points sorted).
	shuffled := boxPrompts()
	shuffled.Prompts[0], shuffled.Prompts[1] = shuffled.Prompts[1], shuffled.Prompts[0]
	shuffled.Obj = 0
	got = nil
	err = c.Track(context.Background(), "sam2-tiny", "", w, h, frames, shuffled, bytes.NewReader(body), func(png []byte) error {
		got = append(got, png)
		return nil
	}, nil)
	if err != nil || len(got) != frames {
		t.Fatalf("default device: err %v, %d records", err, len(got))
	}
	if gotQuery != "frames=2&h=2&model=sam2-tiny&w=3" || gotPrompts != boxPromptsHeader {
		t.Errorf("default device: ?%s header %s", gotQuery, gotPrompts)
	}
	// The stream parser is Matte's: a short count fails the pass the same way.
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		writeRecords(w, want[:1], true)
	})
	calls := 0
	err = c.Track(context.Background(), "sam2-tiny", "", w, h, frames, boxPrompts(), bytes.NewReader(body), func([]byte) error {
		calls++
		return nil
	}, nil)
	if !errors.Is(err, ErrShortCount) || calls != 1 {
		t.Errorf("short stream: err %v after %d records", err, calls)
	}
	// A 503 while the tracker loads is a StatusError with the retry delay.
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
		io.WriteString(w, `{"error":"model loading","retryAfterMs":800}`)
	})
	err = c.Track(context.Background(), "sam2-tiny", "", w, h, frames, boxPrompts(), bytes.NewReader(body), func([]byte) error { return nil }, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.RetryAfterMS != 800 {
		t.Errorf("503 track: %v", err)
	}
	// The record cap follows the frame's pixel count, not a square.
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte{0xff, 0xff, 0xff, 0xff})
	})
	err = c.Track(context.Background(), "sam2-tiny", "", w, h, frames, boxPrompts(), bytes.NewReader(body), func([]byte) error { return nil }, nil)
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("oversized record: %v", err)
	}
}

func TestTrackBadArgsMakeNoRequest(t *testing.T) {
	hits := 0
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) { hits++ })
	ctx := context.Background()
	each := func([]byte) error { return nil }
	body := bytes.NewReader(nil)
	ok := boxPrompts()
	if err := c.Track(ctx, "", "", 4, 4, 1, ok, body, each, nil); err == nil {
		t.Error("empty model accepted")
	}
	if err := c.Track(ctx, "sam2-tiny", "", 0, 4, 1, ok, body, each, nil); err == nil {
		t.Error("width 0 accepted")
	}
	if err := c.Track(ctx, "sam2-tiny", "", 4, 4, 0, ok, body, each, nil); err == nil {
		t.Error("frames 0 accepted")
	}
	if err := c.Track(ctx, "sam2-tiny", "", 4, 4, 1, ok, body, nil, nil); err == nil {
		t.Error("nil callback accepted")
	}
	if err := c.Track(ctx, "sam2-tiny", "", 4, 4, 1, TrackPrompts{}, body, each, nil); err == nil {
		t.Error("no prompts accepted")
	}
	negOnly := TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 0}}}}}
	if err := c.Track(ctx, "sam2-tiny", "", 4, 4, 1, negOnly, body, each, nil); err == nil {
		t.Error("a negative-only prompt set accepted")
	}
	frame := make([]byte, 4*4*3)
	if _, err := c.TrackFrame(ctx, "", "", 4, 4, ok, frame, nil); err == nil {
		t.Error("track frame: empty model accepted")
	}
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", 4, 0, ok, frame, nil); err == nil {
		t.Error("track frame: height 0 accepted")
	}
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", 4, 4, ok, frame[:40], nil); err == nil {
		t.Error("track frame: a short frame accepted")
	}
	if _, err := c.TrackFrame(ctx, "sam2-tiny", "", 4, 4, TrackPrompts{}, frame, nil); err == nil {
		t.Error("track frame: no prompts accepted")
	}
	if hits != 0 {
		t.Errorf("%d requests made", hits)
	}
}

func TestTrackFrame(t *testing.T) {
	const w, h = 2, 2
	frame := make([]byte, TrackBodyLength(1, w, h))
	for i := range frame {
		frame[i] = byte(200 - i)
	}
	mask := fakePNG(7)
	var gotMethod, gotPath, gotQuery, gotType, gotPrompts string
	var gotLen int64
	var gotBody []byte
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotType, gotLen, gotPrompts = r.Header.Get("Content-Type"), r.ContentLength, r.Header.Get(PromptsHeader)
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "image/png")
		w.Write(mask)
	})
	only := boxPrompts().ForFrame(12)
	got, err := c.TrackFrame(context.Background(), "sam2-tiny", DeviceCPU, w, h, only, frame, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/track/frame" || gotQuery != "device=cpu&h=2&model=sam2-tiny&w=2" {
		t.Errorf("request %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if gotType != ContentType || gotLen != 12 || !bytes.Equal(gotBody, frame) {
		t.Errorf("body: type %q len %d equal %v", gotType, gotLen, bytes.Equal(gotBody, frame))
	}
	const wantHdr = `{"obj":1,"prompts":[{"frame":12,"points":[[0.5,0.5,1]],"box":null}]}`
	if gotPrompts != wantHdr {
		t.Errorf("%s header:\n got %s\nwant %s", PromptsHeader, gotPrompts, wantHdr)
	}
	if !bytes.Equal(got, mask) {
		t.Errorf("mask %q, want %q", got, mask)
	}
	// Default device: no device=.
	if _, err := c.TrackFrame(context.Background(), "sam2-tiny", "", w, h, only, frame, nil); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "h=2&model=sam2-tiny&w=2" {
		t.Errorf("default device: ?%s", gotQuery)
	}
	// Loading tracker → StatusError with the delay; empty and oversized answers are stream errors.
	status, body := 503, []byte(`{"error":"model loading","retryAfterMs":600}`)
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		w.Write(body)
	})
	_, err = c.TrackFrame(context.Background(), "sam2-tiny", "", w, h, only, frame, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.RetryAfterMS != 600 {
		t.Errorf("503 track frame: %v", err)
	}
	status, body = 200, nil
	if _, err = c.TrackFrame(context.Background(), "sam2-tiny", "", w, h, only, frame, nil); !errors.Is(err, ErrTruncated) {
		t.Errorf("empty mask: %v", err)
	}
	body = bytes.Repeat([]byte{1}, int(recordCap(w*h))+1)
	if _, err = c.TrackFrame(context.Background(), "sam2-tiny", "", w, h, only, frame, nil); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("oversized mask: %v", err)
	}
}

func TestPromptsCanonical(t *testing.T) {
	p := boxPrompts()
	if got := p.Canonical(); got != boxPromptsCanonical {
		t.Errorf("Canonical:\n got %s\nwant %s", got, boxPromptsCanonical)
	}
	// Prompt order, click order, Obj 0 and sub-rounding noise leave the key alone; the receiver is untouched.
	q := TrackPrompts{Obj: 0, Prompts: []FramePrompt{
		{Frame: 12, Points: [][3]float64{{0.50004, 0.49996, 1}}},
		{Frame: 0, Points: [][3]float64{{0.7, 0.4, 0}}, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}},
	}}
	if got := q.Canonical(); got != boxPromptsCanonical {
		t.Errorf("reordered / rounded Canonical:\n got %s\nwant %s", got, boxPromptsCanonical)
	}
	if q.Prompts[0].Frame != 12 || q.Prompts[0].Points[0][0] != 0.50004 || q.Obj != 0 {
		t.Error("Canonical modified its receiver")
	}
	multi := TrackPrompts{Obj: 2, Prompts: []FramePrompt{
		{Frame: 3, Points: [][3]float64{{0.9, 0.1, 1}, {0.2, 0.8, 0}, {0.2, 0.3, 1}, {0.2, 0.3, 0}}},
	}}
	const wantMulti = "obj=2|f=3:box=-:pts=0.2000,0.3000,0;0.2000,0.3000,1;0.2000,0.8000,0;0.9000,0.1000,1"
	if got := multi.Canonical(); got != wantMulti {
		t.Errorf("points sorted:\n got %s\nwant %s", got, wantMulti)
	}
	// Negative zero folds; rounding is to 4 decimals, half away from zero; a box-only frame has an empty pts.
	z := TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{math.Copysign(0, -1), 0.123456, 0.99995, 1}}}}
	const wantZ = "obj=1|f=0:box=0.0000,0.1235,1.0000,1.0000:pts="
	if got := z.Canonical(); got != wantZ {
		t.Errorf("rounding:\n got %s\nwant %s", got, wantZ)
	}
	if (TrackPrompts{}).Canonical() != "obj=1" {
		t.Errorf("empty Canonical = %q", (TrackPrompts{}).Canonical())
	}
	// Every change of the picture changes the string.
	variants := []TrackPrompts{
		{Obj: 3, Prompts: boxPrompts().Prompts},
		{Prompts: []FramePrompt{{Frame: 1, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}, Points: [][3]float64{{0.7, 0.4, 0}}}, boxPrompts().Prompts[1]}},
		{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0.1, 0.2, 0.6, 0.9}, Points: [][3]float64{{0.7, 0.4, 1}}}, boxPrompts().Prompts[1]}},
		{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0.1, 0.2, 0.6, 0.9001}, Points: [][3]float64{{0.7, 0.4, 0}}}, boxPrompts().Prompts[1]}},
		{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{0.7, 0.4, 0}}}, boxPrompts().Prompts[1]}},
		{Prompts: boxPrompts().Prompts[:1]},
	}
	seen := map[string]int{boxPromptsCanonical: -1}
	for i, v := range variants {
		s := v.Canonical()
		if prev, dup := seen[s]; dup {
			t.Errorf("variant %d renders like %d: %s", i, prev, s)
		}
		seen[s] = i
	}
	// The wire header carries the same rounding and normalisation.
	hdr, err := q.headerValue()
	if err != nil || hdr != boxPromptsHeader {
		t.Errorf("headerValue: %v\n got %s\nwant %s", err, hdr, boxPromptsHeader)
	}
	hdr, err = z.headerValue()
	if err != nil || hdr != `{"obj":1,"prompts":[{"frame":0,"points":[],"box":[0,0.1235,1,1]}]}` {
		t.Errorf("box-only headerValue: %v %s", err, hdr)
	}
	if _, err := (TrackPrompts{}).headerValue(); err == nil {
		t.Error("headerValue of no prompts")
	}
	// ForFrame.
	f0 := boxPrompts().ForFrame(0)
	if f0.Obj != 1 || len(f0.Prompts) != 1 || f0.Prompts[0].Frame != 0 || f0.Prompts[0].Box == nil {
		t.Errorf("ForFrame(0) = %+v", f0)
	}
	if none := boxPrompts().ForFrame(5); none.Prompts != nil || none.Obj != 1 {
		t.Errorf("ForFrame(5) = %+v", none)
	}
	if err := boxPrompts().ForFrame(0).Validate(); err != nil {
		t.Errorf("frame 0 alone (a box): %v", err)
	}
}

func TestPromptsValidate(t *testing.T) {
	box := &[4]float64{0.1, 0.2, 0.6, 0.9}
	pos := [][3]float64{{0.5, 0.5, 1}}
	neg := [][3]float64{{0.5, 0.5, 0}}
	cases := []struct {
		name string
		p    TrackPrompts
		ok   bool
	}{
		{"box only", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: box}}}, true},
		{"positive point only", TrackPrompts{Prompts: []FramePrompt{{Frame: 4, Points: pos}}}, true},
		{"negative on one frame, box on another", TrackPrompts{Prompts: []FramePrompt{{Frame: 4, Points: neg}, {Frame: 0, Box: box}}}, true},
		{"corner coordinates", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0, 0, 1, 1}, Points: [][3]float64{{0, 1, 1}}}}}, true},
		{"none", TrackPrompts{}, false},
		{"negative obj", TrackPrompts{Obj: -1, Prompts: []FramePrompt{{Frame: 0, Box: box}}}, false},
		{"negative frame", TrackPrompts{Prompts: []FramePrompt{{Frame: -1, Box: box}}}, false},
		{"neither box nor point", TrackPrompts{Prompts: []FramePrompt{{Frame: 0}}}, false},
		{"empty prompt beside a good one", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: box}, {Frame: 1}}}, false},
		{"negative only", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: neg}}}, false},
		{"box outside 0..1", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{-0.1, 0.2, 0.6, 0.9}}}}, false},
		{"box over 1", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0.1, 0.2, 1.6, 0.9}}}}, false},
		{"box inverted", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0.6, 0.2, 0.1, 0.9}}}}, false},
		{"box empty", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0.5, 0.2, 0.5, 0.9}}}}, false},
		{"point outside", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{1.5, 0.5, 1}}}}}, false},
		{"point label 2", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 2}}}}}, false},
		{"point label 0.5", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{0.5, 0.5, 0.5}}}}}, false},
		{"NaN coordinate", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Points: [][3]float64{{math.NaN(), 0.5, 1}}}}}, false},
		{"NaN in the box", TrackPrompts{Prompts: []FramePrompt{{Frame: 0, Box: &[4]float64{0.1, math.NaN(), 0.6, 0.9}}}}, false},
	}
	for _, tc := range cases {
		err := tc.p.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

func TestClipKeyTracker(t *testing.T) {
	base := goldenParts()
	baseKey := ClipKey(base)
	// Zero tracker parts are the pre-5c text — the pinned golden of TestClipKeyGolden.
	same := goldenParts()
	same.Prompts, same.TrackW, same.TrackH = "", 0, 0
	if ClipKey(same) != baseKey {
		t.Error("zero tracker parts changed a segmenter's key")
	}
	tr := goldenParts()
	tr.Model, tr.Size, tr.Precision, tr.Weights = "sam2-tiny", 0, "bf16", strings.Repeat("74", 32)
	tr.Prompts, tr.TrackW, tr.TrackH = boxPrompts().Canonical(), 1024, 576
	text := clipKeyText(tr)
	const wantTail = "\nfps=25|model=sam2-tiny|size=0|prec=bf16|weights=7474747474747474747474747474747474747474747474747474747474747474|proc=1" +
		"\ntrack=1024x576|prompts=" + boxPromptsCanonical
	if !strings.HasSuffix(text, wantTail) {
		t.Errorf("tracker key text tail:\n got %q\nwant suffix %q", text, wantTail)
	}
	if !strings.HasPrefix(text, "matte|1\n") {
		t.Errorf("tracker key text does not start with the version line: %q", text)
	}
	sum := sha256.Sum256([]byte(text))
	if got := ClipKey(tr); got != hex.EncodeToString(sum[:]) {
		t.Errorf("ClipKey = %s, not the sha256 of its text", got)
	}
	// Pinned: a change here orphans every memoised tracker matte.
	const golden = "5f11c197d30b487a2e7b97e7aea087694c366b4995e906a1134e51683d02938a"
	if got := ClipKey(tr); got != golden {
		t.Errorf("tracker ClipKey golden:\n got %s\nwant %s", got, golden)
	}
	// Each tracker part changes the key, and the prompts' order does not.
	keys := map[string]string{ClipKey(tr): "tracker"}
	for _, tc := range []struct {
		name string
		mut  func(*ClipKeyParts)
	}{
		{"prompts", func(p *ClipKeyParts) { p.Prompts = boxPrompts().ForFrame(0).Canonical() }},
		{"track width", func(p *ClipKeyParts) { p.TrackW = 1000 }},
		{"track height", func(p *ClipKeyParts) { p.TrackH = 562 }},
		{"no prompts", func(p *ClipKeyParts) { p.Prompts = "" }},
		{"no size", func(p *ClipKeyParts) { p.TrackW, p.TrackH = 0, 0 }},
	} {
		p := tr
		tc.mut(&p)
		k := ClipKey(p)
		if prev, dup := keys[k]; dup {
			t.Errorf("%s: key equals %s", tc.name, prev)
		}
		keys[k] = tc.name
	}
	shuffled := boxPrompts()
	shuffled.Prompts[0], shuffled.Prompts[1] = shuffled.Prompts[1], shuffled.Prompts[0]
	p := tr
	p.Prompts = shuffled.Canonical()
	if ClipKey(p) != ClipKey(tr) {
		t.Error("prompt order changed the key")
	}
}

func TestFacts5cRoundTrip(t *testing.T) {
	p := parsePing(t, ping5cJSON)
	dir := t.TempDir()
	path := filepath.Join(dir, "mattes", FactsName)
	if err := SaveFacts(path, p); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Ping, *p) {
		t.Errorf("5c ping round trip:\n got %+v\nwant %+v", f.Ping, *p)
	}
	isnet, _ := f.Model("isnet-anime")
	if d, ok := isnet.On(DeviceCPU); !ok || d.Precision != "fp32" || d.EffectiveSize(0) != 512 || d.MsPerFrameAt(0) != 136 {
		t.Errorf("per-device facts after a reload: %v %+v", ok, d)
	}
	if sam, _ := f.Model("sam2-tiny"); !sam.IsTracker() {
		t.Error("tracker kind lost in the facts file")
	}
	if f.DefaultModelFor(DeviceCPU) != "isnet-anime" || !f.Offers(DeviceCPU) {
		t.Error("device facts lost in the facts file")
	}
	// The file carries the new fields by name (a tool reading it sees them).
	raw, _ := os.ReadFile(path)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"devices", "defaultDevice", "defaultModels"} {
		if _, ok := top[k]; !ok {
			t.Errorf("facts file lacks %q", k)
		}
	}
	var models map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["models"], &models); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"kind", "devices", "state", "msPerFrame"} {
		if _, ok := models["isnet-anime"][k]; !ok {
			t.Errorf("facts model lacks %q", k)
		}
	}
	if _, ok := models["isnet-anime"]["resident"]; ok {
		t.Error("a false resident flag is written (omitempty expected)")
	}
	if _, ok := models["birefnet-lite"]["resident"]; !ok {
		t.Error("a true resident flag is not written")
	}
	// A facts file written by the 5b app (no device fields) still loads under FactsVersion 1.
	old := parsePing(t, pingJSON)
	if err := SaveFacts(path, old); err != nil {
		t.Fatal(err)
	}
	f, err = LoadFacts(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Devices != nil || f.DefaultModels != nil || !f.Offers("") || f.EffectiveDevice("") != DeviceCUDA {
		t.Errorf("5b facts: %+v", f.Ping)
	}
	raw, _ = os.ReadFile(path)
	if bytes.Contains(raw, []byte(`"devices"`)) || bytes.Contains(raw, []byte(`"kind"`)) {
		t.Error("5b facts file grew empty 5c fields")
	}
}

func TestManifestTrackerFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clip", ManifestName)
	m := Manifest{Key: strings.Repeat("ab", 32), Src: strings.Repeat("cd", 32), Model: "sam2-tiny",
		Weights: strings.Repeat("74", 32), Proc: "1", Precision: "bf16", FPS: "25", Frames: 45, Device: "cuda",
		MsPerFrame: 30, Prompts: boxPromptsCanonical, TrackW: 1024, TrackH: 576}
	if err := WriteManifest(path, &m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompts != boxPromptsCanonical || got.TrackW != 1024 || got.TrackH != 576 || got.Size != 0 {
		t.Errorf("tracker manifest: %+v", *got)
	}
	// A segmenter's manifest does not carry the tracker fields.
	seg := Manifest{Key: "k", Src: "s", Model: "isnet-anime", Size: 1024, FPS: "25", Frames: 3}
	if err := WriteManifest(path, &seg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, k := range []string{`"prompts"`, `"trackW"`, `"trackH"`} {
		if bytes.Contains(raw, []byte(k)) {
			t.Errorf("segmenter manifest carries %s", k)
		}
	}
}

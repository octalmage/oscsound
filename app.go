package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/gopxl/beep/v2/wav"
	"github.com/hypebeast/go-osc/osc"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	speakerRate = beep.SampleRate(44100)
	TypeOneShot = "oneshot"
	TypeLoop    = "loop"
)

type Sound struct {
	Name   string  `json:"name"`
	Param  string  `json:"param"`
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	Volume float64 `json:"volume"`
}

// Default Volume to 1.0 when the field is absent so older configs (and packs)
// don't silently mute themselves.
func (s *Sound) UnmarshalJSON(data []byte) error {
	type alias Sound
	a := alias{Volume: 1.0}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = Sound(a)
	return nil
}

type Config struct {
	Sounds []Sound `json:"sounds"`
}

type Status struct {
	Port     int  `json:"port"`
	OSCQuery bool `json:"oscQuery"`
}

type gainStreamer struct {
	s beep.Streamer
	g atomic.Uint64 // float64 bits
}

func newGain(s beep.Streamer, v float64) *gainStreamer {
	g := &gainStreamer{s: s}
	g.set(v)
	return g
}

func (g *gainStreamer) set(v float64) { g.g.Store(math.Float64bits(clamp01(v))) }

func (g *gainStreamer) Stream(samples [][2]float64) (int, bool) {
	gv := math.Float64frombits(g.g.Load())
	n, ok := g.s.Stream(samples)
	for i := 0; i < n; i++ {
		samples[i][0] *= gv
		samples[i][1] *= gv
	}
	return n, ok
}

func (g *gainStreamer) Err() error { return g.s.Err() }

type loopHandle struct {
	gain *gainStreamer
	done atomic.Bool
}

func (l *loopHandle) Stream(samples [][2]float64) (int, bool) {
	if l.done.Load() {
		return 0, false
	}
	return l.gain.Stream(samples)
}

func (l *loopHandle) Err() error { return l.gain.Err() }

type previewStream struct {
	gain  *gainStreamer
	done  atomic.Bool
	ended atomic.Bool
	onEnd func()
}

func (p *previewStream) Stream(samples [][2]float64) (int, bool) {
	if p.done.Load() {
		p.fireEnd()
		return 0, false
	}
	n, ok := p.gain.Stream(samples)
	if !ok {
		p.fireEnd()
	}
	return n, ok
}

func (p *previewStream) Err() error { return p.gain.Err() }

func (p *previewStream) fireEnd() {
	if p.ended.CompareAndSwap(false, true) {
		p.onEnd()
	}
}

type App struct {
	ctx context.Context

	mu       sync.Mutex
	cfg      Config
	cache    map[string]*beep.Buffer
	loops    map[string]*loopHandle
	previews map[uint64]*previewStream

	mixer     *beep.Mixer
	server    *osc.Server
	oscConn   net.PacketConn
	oscq      *oscquery
	port      int
	nextToken atomic.Uint64
}

func NewApp() *App {
	return &App{
		cache:    map[string]*beep.Buffer{},
		loops:    map[string]*loopHandle{},
		previews: map[uint64]*previewStream{},
		mixer:    &beep.Mixer{},
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	if err := speaker.Init(speakerRate, speakerRate.N(time.Second/20)); err != nil {
		runtime.LogErrorf(ctx, "speaker init: %v", err)
		return
	}
	speaker.Play(a.mixer)

	a.cfg = a.loadConfig()
	a.startOSC()
}

func (a *App) shutdown(context.Context) {
	a.stopAllLoops()
	a.stopAllPreviews()
	if a.oscq != nil {
		a.oscq.Shutdown()
	} else if a.oscConn != nil {
		a.oscConn.Close()
	}
}

// --- exposed to the frontend ---

func (a *App) GetConfig() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) GetStatus() Status {
	return Status{Port: a.port, OSCQuery: a.oscq != nil}
}

func (a *App) SaveConfig(c Config) error {
	for i := range c.Sounds {
		if c.Sounds[i].Type == "" {
			c.Sounds[i].Type = TypeOneShot
		}
	}
	a.mu.Lock()
	a.cfg = c
	a.mu.Unlock()
	a.reconcileLoops(c)
	return a.writeConfig(c)
}

func (a *App) PickFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose a sound",
		Filters: []runtime.FileFilter{
			{DisplayName: "Audio (*.wav, *.mp3)", Pattern: "*.wav;*.mp3"},
		},
	})
}

func (a *App) TestStart(path string, isLoop bool, volume float64) (uint64, error) {
	if path == "" {
		return 0, fmt.Errorf("empty path")
	}
	buf, err := a.getBuffer(path)
	if err != nil {
		return 0, err
	}

	var src beep.Streamer = buf.Streamer(0, buf.Len())
	if isLoop {
		src = beep.Loop(-1, buf.Streamer(0, buf.Len()))
	}
	gain := newGain(src, volume)

	token := a.nextToken.Add(1)
	p := &previewStream{gain: gain}
	p.onEnd = func() {
		a.mu.Lock()
		delete(a.previews, token)
		a.mu.Unlock()
		runtime.EventsEmit(a.ctx, "preview-end", token)
	}

	a.mu.Lock()
	a.previews[token] = p
	a.mu.Unlock()
	speaker.Lock()
	a.mixer.Add(p)
	speaker.Unlock()
	return token, nil
}

func (a *App) TestStop(token uint64) {
	a.mu.Lock()
	p := a.previews[token]
	delete(a.previews, token)
	a.mu.Unlock()
	if p != nil {
		p.done.Store(true)
	}
}

func (a *App) SetPreviewVolume(token uint64, volume float64) {
	a.mu.Lock()
	p := a.previews[token]
	a.mu.Unlock()
	if p != nil {
		p.gain.set(volume)
	}
}

func (a *App) SetLoopVolume(param string, volume float64) {
	a.mu.Lock()
	h := a.loops[param]
	a.mu.Unlock()
	if h != nil {
		h.gain.set(volume)
	}
}

// --- OSC / OSCQuery ---

func (a *App) startOSC() {
	d := osc.NewStandardDispatcher()
	d.AddMsgHandler("*", a.handleMessage)
	a.server = &osc.Server{Dispatcher: d}

	conn := a.bindOSC()
	if conn == nil {
		return
	}
	go func() {
		if err := a.server.Serve(conn); err != nil {
			runtime.LogErrorf(a.ctx, "osc: %v", err)
		}
	}()
}

func (a *App) bindOSC() net.PacketConn {
	name := fmt.Sprintf("oscsound-%d", os.Getpid())
	if q, err := startOSCQuery(name); err == nil {
		a.oscq = q
		a.port = q.OSCPort()
		runtime.LogInfof(a.ctx, "OSCQuery on UDP %d (HTTP %d)", q.OSCPort(), q.HTTPPort())
		return q.oscConn
	} else {
		runtime.LogInfof(a.ctx, "OSCQuery unavailable (%v); using fixed UDP 9001", err)
	}

	conn, err := net.ListenPacket("udp", "127.0.0.1:9001")
	if err != nil {
		runtime.LogErrorf(a.ctx, "osc listen :9001: %v", err)
		return nil
	}
	a.oscConn = conn
	a.port = 9001
	return conn
}

func (a *App) handleMessage(msg *osc.Message) {
	const prefix = "/avatar/parameters/"
	if !strings.HasPrefix(msg.Address, prefix) {
		return
	}
	param := strings.TrimPrefix(msg.Address, prefix)
	on := truthy(msg.Arguments)

	a.mu.Lock()
	var found *Sound
	for i := range a.cfg.Sounds {
		if a.cfg.Sounds[i].Param == param {
			s := a.cfg.Sounds[i]
			found = &s
			break
		}
	}
	a.mu.Unlock()
	if found == nil {
		return
	}

	if found.Type == TypeLoop {
		if on {
			runtime.EventsEmit(a.ctx, "loop-on", param)
			if err := a.startLoop(param, found.Path, found.Volume); err != nil {
				runtime.LogErrorf(a.ctx, "loop %s: %v", found.Path, err)
			}
		} else {
			a.stopLoop(param)
			runtime.EventsEmit(a.ctx, "loop-off", param)
		}
		return
	}

	if on {
		runtime.EventsEmit(a.ctx, "trigger", param)
		if err := a.playOnce(found.Path, found.Volume); err != nil {
			runtime.LogErrorf(a.ctx, "play %s: %v", found.Path, err)
		}
	}
}

// --- playback internals ---

func (a *App) playOnce(path string, volume float64) error {
	buf, err := a.getBuffer(path)
	if err != nil {
		return err
	}
	g := newGain(buf.Streamer(0, buf.Len()), volume)
	speaker.Lock()
	a.mixer.Add(g)
	speaker.Unlock()
	return nil
}

func (a *App) startLoop(param, path string, volume float64) error {
	a.mu.Lock()
	if _, exists := a.loops[param]; exists {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	buf, err := a.getBuffer(path)
	if err != nil {
		return err
	}
	gain := newGain(beep.Loop(-1, buf.Streamer(0, buf.Len())), volume)
	h := &loopHandle{gain: gain}

	a.mu.Lock()
	a.loops[param] = h
	a.mu.Unlock()
	speaker.Lock()
	a.mixer.Add(h)
	speaker.Unlock()
	return nil
}

func (a *App) stopLoop(param string) {
	a.mu.Lock()
	h := a.loops[param]
	delete(a.loops, param)
	a.mu.Unlock()
	if h != nil {
		h.done.Store(true)
	}
}

func (a *App) stopAllLoops() {
	a.mu.Lock()
	hs := a.loops
	a.loops = map[string]*loopHandle{}
	a.mu.Unlock()
	for _, h := range hs {
		h.done.Store(true)
	}
}

func (a *App) stopAllPreviews() {
	a.mu.Lock()
	ps := a.previews
	a.previews = map[uint64]*previewStream{}
	a.mu.Unlock()
	for _, p := range ps {
		p.done.Store(true)
	}
}

func (a *App) reconcileLoops(c Config) {
	keep := map[string]bool{}
	for _, s := range c.Sounds {
		if s.Type == TypeLoop {
			keep[s.Param] = true
		}
	}
	a.mu.Lock()
	var toStop []*loopHandle
	var stopped []string
	for param, h := range a.loops {
		if !keep[param] {
			toStop = append(toStop, h)
			stopped = append(stopped, param)
			delete(a.loops, param)
		}
	}
	a.mu.Unlock()
	for _, h := range toStop {
		h.done.Store(true)
	}
	for _, p := range stopped {
		runtime.EventsEmit(a.ctx, "loop-off", p)
	}
}

func (a *App) getBuffer(path string) (*beep.Buffer, error) {
	a.mu.Lock()
	if b, ok := a.cache[path]; ok {
		a.mu.Unlock()
		return b, nil
	}
	a.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	streamer, format, err := decode(path, f)
	if err != nil {
		f.Close()
		return nil, err
	}
	defer streamer.Close()

	var src beep.Streamer = streamer
	if format.SampleRate != speakerRate {
		src = beep.Resample(4, format.SampleRate, speakerRate, streamer)
	}
	buf := beep.NewBuffer(beep.Format{
		SampleRate:  speakerRate,
		NumChannels: format.NumChannels,
		Precision:   format.Precision,
	})
	buf.Append(src)

	a.mu.Lock()
	a.cache[path] = buf
	a.mu.Unlock()
	return buf, nil
}

// --- packs ---

type packManifest struct {
	Version int     `json:"version"`
	Name    string  `json:"name,omitempty"`
	Sounds  []Sound `json:"sounds"`
}

func (a *App) ExportPack() (string, error) {
	dst, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Export pack",
		DefaultFilename: "oscsound-pack.zip",
		Filters:         []runtime.FileFilter{{DisplayName: "Pack (*.zip)", Pattern: "*.zip"}},
	})
	if err != nil || dst == "" {
		return "", err
	}

	a.mu.Lock()
	sounds := append([]Sound(nil), a.cfg.Sounds...)
	a.mu.Unlock()

	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	defer zw.Close()

	manifest := packManifest{Version: 1}
	used := map[string]int{}
	for _, s := range sounds {
		if s.Path == "" {
			continue
		}
		base := filepath.Base(s.Path)
		if n := used[base]; n > 0 {
			ext := filepath.Ext(base)
			base = strings.TrimSuffix(base, ext) + fmt.Sprintf("-%d", n) + ext
		}
		used[filepath.Base(s.Path)]++

		w, err := zw.Create("sounds/" + base)
		if err != nil {
			return "", err
		}
		if err := copyFileTo(w, s.Path); err != nil {
			return "", err
		}

		manifest.Sounds = append(manifest.Sounds, Sound{
			Name:   s.Name,
			Param:  s.Param,
			Type:   normType(s.Type),
			Path:   base,
			Volume: s.Volume,
		})
	}

	mw, err := zw.Create("manifest.json")
	if err != nil {
		return "", err
	}
	if err := json.NewEncoder(mw).Encode(manifest); err != nil {
		return "", err
	}
	return dst, nil
}

func (a *App) ImportPack() error {
	src, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Import pack",
		Filters: []runtime.FileFilter{{DisplayName: "Pack (*.zip)", Pattern: "*.zip"}},
	})
	if err != nil || src == "" {
		return err
	}

	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	var manifest packManifest
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = json.NewDecoder(rc).Decode(&manifest)
		rc.Close()
		if err != nil {
			return fmt.Errorf("manifest: %w", err)
		}
		break
	}
	if len(manifest.Sounds) == 0 {
		return fmt.Errorf("no sounds in pack")
	}

	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	dest := filepath.Join(packsDir(), fmt.Sprintf("%d-%s", time.Now().Unix(), sanitize(stem)))
	if err := os.MkdirAll(filepath.Join(dest, "sounds"), 0o755); err != nil {
		return err
	}

	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "sounds/") || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(filepath.Join(dest, "sounds", filepath.Base(f.Name)), rc)
		rc.Close()
		if err != nil {
			return err
		}
	}

	a.mu.Lock()
	for _, s := range manifest.Sounds {
		a.cfg.Sounds = append(a.cfg.Sounds, Sound{
			Name:   s.Name,
			Param:  s.Param,
			Type:   normType(s.Type),
			Path:   filepath.Join(dest, "sounds", filepath.Base(s.Path)),
			Volume: s.Volume,
		})
	}
	cfg := a.cfg
	a.mu.Unlock()
	return a.writeConfig(cfg)
}

// --- config I/O ---

func (a *App) loadConfig() Config {
	var c Config
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func (a *App) writeConfig(c Config) error {
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o644)
}

// --- helpers ---

func decode(path string, f *os.File) (beep.StreamSeekCloser, beep.Format, error) {
	if strings.EqualFold(filepath.Ext(path), ".mp3") {
		return mp3.Decode(f)
	}
	return wav.Decode(f)
}

func truthy(args []interface{}) bool {
	if len(args) == 0 {
		return true
	}
	switch v := args[0].(type) {
	case bool:
		return v
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float32:
		return v > 0.5
	case float64:
		return v > 0.5
	}
	return false
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func normType(t string) string {
	if t == TypeLoop {
		return TypeLoop
	}
	return TypeOneShot
}

func sanitize(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(s)
}

func dataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "oscsound")
}

func configPath() string { return filepath.Join(dataDir(), "config.json") }
func packsDir() string   { return filepath.Join(dataDir(), "packs") }

func copyFileTo(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

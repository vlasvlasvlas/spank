// spank detects slaps/hits on the laptop and plays audio responses.
// It reads the Apple Silicon accelerometer directly via IOKit HID —
// no separate sensor daemon required. Needs sudo.
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/effects"
	"github.com/gopxl/beep/v2/generators"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/gopxl/beep/v2/wav"
	"github.com/spf13/cobra"
	"github.com/taigrr/apple-silicon-accelerometer/detector"
	"github.com/taigrr/apple-silicon-accelerometer/sensor"
	"github.com/taigrr/apple-silicon-accelerometer/shm"
	"gopkg.in/yaml.v3"
)

var version = "dev"

//go:embed audio/pain/*.mp3
var painAudio embed.FS

//go:embed audio/sexy/*.mp3
var sexyAudio embed.FS

//go:embed audio/halo/*.mp3
var haloAudio embed.FS

//go:embed audio/lizard/*.mp3
var lizardAudio embed.FS

//go:embed web/*
var webAssets embed.FS

var (
	sexyMode       bool
	haloMode       bool
	lizardMode     bool
	customPath     string
	customFiles    []string
	fastMode       bool
	minAmplitude   float64
	cooldownMs     int
	stdioMode      bool
	volumeScaling  bool
	paused         bool
	pausedMu       sync.RWMutex
	speedRatio     float64
	instrumentMode bool
	configPath     string
	webAddr        string
)

// sensorReady is closed once shared memory is created and the sensor
// worker is about to enter the CFRunLoop.
var sensorReady = make(chan struct{})

// sensorErr receives any error from the sensor worker.
var sensorErr = make(chan error, 1)

type playMode int

const (
	modeRandom playMode = iota
	modeEscalation
)

const (
	// decayHalfLife is how many seconds of inactivity before intensity
	// halves. Controls how fast escalation fades.
	decayHalfLife = 30.0

	// defaultMinAmplitude is the default detection threshold.
	defaultMinAmplitude = 0.05

	// defaultCooldownMs is the default cooldown between audio responses.
	defaultCooldownMs = 750

	// defaultSpeedRatio is the default playback speed (1.0 = normal).
	defaultSpeedRatio = 1.0

	// defaultSensorPollInterval is how often we check for new accelerometer data.
	defaultSensorPollInterval = 10 * time.Millisecond

	// defaultMaxSampleBatch caps the number of accelerometer samples processed
	// per tick to avoid falling behind.
	defaultMaxSampleBatch = 200

	// sensorStartupDelay gives the sensor time to start producing data.
	sensorStartupDelay = 100 * time.Millisecond
)

type runtimeTuning struct {
	minAmplitude float64
	cooldown     time.Duration
	pollInterval time.Duration
	maxBatch     int
}

func defaultTuning() runtimeTuning {
	return runtimeTuning{
		minAmplitude: defaultMinAmplitude,
		cooldown:     time.Duration(defaultCooldownMs) * time.Millisecond,
		pollInterval: defaultSensorPollInterval,
		maxBatch:     defaultMaxSampleBatch,
	}
}

func applyFastOverlay(base runtimeTuning) runtimeTuning {
	base.pollInterval = 4 * time.Millisecond
	base.cooldown = 350 * time.Millisecond
	if base.minAmplitude > 0.18 {
		base.minAmplitude = 0.18
	}
	if base.maxBatch < 320 {
		base.maxBatch = 320
	}
	return base
}

type soundPack struct {
	name   string
	fs     embed.FS
	dir    string
	mode   playMode
	files  []string
	custom bool
}

func (sp *soundPack) loadFiles() error {
	if sp.custom {
		entries, err := os.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && isSupportedAudioFile(entry.Name()) {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	} else {
		entries, err := sp.fs.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() && isSupportedAudioFile(entry.Name()) {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	}
	sort.Strings(sp.files)
	if len(sp.files) == 0 {
		return fmt.Errorf("no audio files found in %s", sp.dir)
	}
	return nil
}

func isSupportedAudioFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp3", ".wav":
		return true
	default:
		return false
	}
}

type instrumentConfig struct {
	Title  string                 `json:"title" yaml:"title"`
	Web    instrumentWebConfig    `json:"web" yaml:"web"`
	Input  instrumentInputConfig  `json:"input" yaml:"input"`
	Audio  instrumentAudioConfig  `json:"audio" yaml:"audio"`
	Visual instrumentVisualConfig `json:"visual" yaml:"visual"`
}

type instrumentWebConfig struct {
	Title string `json:"title" yaml:"title"`
	Addr  string `json:"addr" yaml:"addr"`
}

type instrumentInputConfig struct {
	MinAmplitude float64 `json:"min_amplitude" yaml:"min_amplitude"`
}

type instrumentAudioConfig struct {
	SamplesEnabled bool                    `json:"samples_enabled" yaml:"samples_enabled"`
	SamplesGain    float64                 `json:"samples_gain" yaml:"samples_gain"`
	MasterGain     float64                 `json:"master_gain" yaml:"master_gain"`
	SynthGain      float64                 `json:"synth_gain" yaml:"synth_gain"`
	RecordingsDir  string                  `json:"recordings_dir" yaml:"recordings_dir"`
	FX             instrumentFXConfig      `json:"fx" yaml:"fx"`
	Synth          instrumentSynthConfig   `json:"synth" yaml:"synth"`
	Layers         []instrumentLayerConfig `json:"layers" yaml:"layers"`
}

type instrumentLayerConfig struct {
	ID         string  `json:"id" yaml:"id"`
	Label      string  `json:"label" yaml:"label"`
	Pack       string  `json:"pack,omitempty" yaml:"pack,omitempty"`
	Path       string  `json:"path,omitempty" yaml:"path,omitempty"`
	Managed    bool    `json:"managed_recording,omitempty" yaml:"-"`
	Trigger    string  `json:"trigger" yaml:"trigger"`
	Family     string  `json:"family" yaml:"family"`
	Gain       float64 `json:"gain" yaml:"gain"`
	Enabled    bool    `json:"enabled" yaml:"enabled"`
	Loop       bool    `json:"loop" yaml:"loop"`
	DelaySend  float64 `json:"delay_send" yaml:"delay_send"`
	ReverbSend float64 `json:"reverb_send" yaml:"reverb_send"`
}

type instrumentFXConfig struct {
	Delay  instrumentDelayConfig  `json:"delay" yaml:"delay"`
	Reverb instrumentReverbConfig `json:"reverb" yaml:"reverb"`
}

type instrumentDelayConfig struct {
	Enabled  bool    `json:"enabled" yaml:"enabled"`
	TimeMs   int     `json:"time_ms" yaml:"time_ms"`
	Feedback float64 `json:"feedback" yaml:"feedback"`
	Mix      float64 `json:"mix" yaml:"mix"`
}

type instrumentReverbConfig struct {
	Enabled    bool    `json:"enabled" yaml:"enabled"`
	PreDelayMs int     `json:"pre_delay_ms" yaml:"pre_delay_ms"`
	Decay      float64 `json:"decay" yaml:"decay"`
	Mix        float64 `json:"mix" yaml:"mix"`
}

type instrumentSynthConfig struct {
	Enabled     bool    `json:"enabled" yaml:"enabled"`
	Family      string  `json:"family" yaml:"family"`
	Wave        string  `json:"wave" yaml:"wave"`
	Frequency   float64 `json:"frequency" yaml:"frequency"`
	PitchFollow float64 `json:"pitch_follow" yaml:"pitch_follow"`
	DurationMs  int     `json:"duration_ms" yaml:"duration_ms"`
	AttackMs    int     `json:"attack_ms" yaml:"attack_ms"`
	ReleaseMs   int     `json:"release_ms" yaml:"release_ms"`
	Gain        float64 `json:"gain" yaml:"gain"`
}

type instrumentVisualConfig struct {
	BasePalette  []string            `json:"base_palette" yaml:"base_palette"`
	SoftPalette  []string            `json:"soft_palette" yaml:"soft_palette"`
	HardPalette  []string            `json:"hard_palette" yaml:"hard_palette"`
	ComboPalette []string            `json:"combo_palette" yaml:"combo_palette"`
	Families     map[string][]string `json:"families" yaml:"families"`
	Impact       instrumentImpact    `json:"impact" yaml:"impact"`
	Combo        instrumentCombo     `json:"combo" yaml:"combo"`
}

type instrumentImpact struct {
	FlashMs            int     `json:"flash_ms" yaml:"flash_ms"`
	FadeMs             int     `json:"fade_ms" yaml:"fade_ms"`
	ColorRGB           []int   `json:"color_rgb" yaml:"color_rgb"`
	FullScaleAmplitude float64 `json:"full_scale_amplitude" yaml:"full_scale_amplitude"`
	RippleScale        float64 `json:"ripple_scale" yaml:"ripple_scale"`
	Shake              float64 `json:"shake" yaml:"shake"`
	Bloom              float64 `json:"bloom" yaml:"bloom"`
}

type instrumentCombo struct {
	ResetMs    int      `json:"reset_ms" yaml:"reset_ms"`
	DecayMs    int      `json:"decay_ms" yaml:"decay_ms"`
	Milestones []int    `json:"milestones" yaml:"milestones"`
	Labels     []string `json:"labels" yaml:"labels"`
}

type instrumentLayer struct {
	cfg          instrumentLayerConfig
	pack         *soundPack
	tracker      *slapTracker
	resolvedPath string
}

type instrumentPlayback struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	File    string  `json:"file"`
	Family  string  `json:"family"`
	Gain    float64 `json:"gain"`
	Trigger string  `json:"trigger"`
}

type instrumentEvent struct {
	Timestamp   string               `json:"timestamp"`
	Amplitude   float64              `json:"amplitude"`
	Severity    string               `json:"severity"`
	Combo       int                  `json:"combo"`
	Energy      float64              `json:"energy"`
	Tier        string               `json:"tier"`
	ActiveLoops int                  `json:"active_loops"`
	Playbacks   []instrumentPlayback `json:"playbacks"`
}

type instrumentLayerPatch struct {
	Gain       *float64 `json:"gain,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
	DelaySend  *float64 `json:"delay_send,omitempty"`
	ReverbSend *float64 `json:"reverb_send,omitempty"`
}

type instrumentMixerPatch struct {
	SamplesEnabled *bool    `json:"samples_enabled,omitempty"`
	SamplesGain    *float64 `json:"samples_gain,omitempty"`
	MasterGain     *float64 `json:"master_gain,omitempty"`
	SynthGain      *float64 `json:"synth_gain,omitempty"`
}

type instrumentInputPatch struct {
	MinAmplitude *float64 `json:"min_amplitude,omitempty"`
}

type instrumentFXPatch struct {
	DelayTimeMs    *int     `json:"delay_time_ms,omitempty"`
	DelayFeedback  *float64 `json:"delay_feedback,omitempty"`
	DelayMix       *float64 `json:"delay_mix,omitempty"`
	DelayEnabled   *bool    `json:"delay_enabled,omitempty"`
	ReverbPreDelay *int     `json:"reverb_pre_delay_ms,omitempty"`
	ReverbDecay    *float64 `json:"reverb_decay,omitempty"`
	ReverbMix      *float64 `json:"reverb_mix,omitempty"`
	ReverbEnabled  *bool    `json:"reverb_enabled,omitempty"`
}

type instrumentSynthPatch struct {
	Enabled     *bool    `json:"enabled,omitempty"`
	Wave        *string  `json:"wave,omitempty"`
	Frequency   *float64 `json:"frequency,omitempty"`
	PitchFollow *float64 `json:"pitch_follow,omitempty"`
	DurationMs  *int     `json:"duration_ms,omitempty"`
	AttackMs    *int     `json:"attack_ms,omitempty"`
	ReleaseMs   *int     `json:"release_ms,omitempty"`
	Gain        *float64 `json:"gain,omitempty"`
}

type instrumentVisualPatch struct {
	ColorRGB           []int    `json:"color_rgb,omitempty"`
	FlashMs            *int     `json:"flash_ms,omitempty"`
	FadeMs             *int     `json:"fade_ms,omitempty"`
	FullScaleAmplitude *float64 `json:"full_scale_amplitude,omitempty"`
}

type instrumentRuntimeState struct {
	Combo       int     `json:"combo"`
	Energy      float64 `json:"energy"`
	Tier        string  `json:"tier"`
	LastHitUnix int64   `json:"last_hit_unix"`
	ActiveLoops int     `json:"active_loops"`
}

type instrumentMeta struct {
	SavePath      string `json:"save_path"`
	Dirty         bool   `json:"dirty"`
	LastSavedUnix int64  `json:"last_saved_unix"`
	LastSaveError string `json:"last_save_error,omitempty"`
}

type instrumentSnapshot struct {
	Config  instrumentConfig       `json:"config"`
	Runtime instrumentRuntimeState `json:"runtime"`
	Meta    instrumentMeta         `json:"meta"`
}

type instrumentSaveRequest struct {
	Path string `json:"path,omitempty"`
}

type instrumentRecordingResponse struct {
	Snapshot instrumentSnapshot `json:"snapshot"`
	LayerID  string             `json:"layer_id"`
	Path     string             `json:"path"`
}

type eventHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

type instrumentState struct {
	mu            sync.RWMutex
	cfg           instrumentConfig
	layers        map[string]*instrumentLayer
	hub           *eventHub
	audio         *instrumentAudioEngine
	savePath      string
	configDir     string
	dirty         bool
	lastSaved     time.Time
	lastSaveError string
	combo         int
	energy        float64
	lastHit       time.Time
}

type instrumentAudioEngine struct {
	mu          sync.Mutex
	initialized bool
	loops       map[string]*instrumentLoopVoice
}

type instrumentLoopVoice struct {
	ctrl        *beep.Ctrl
	closers     []io.Closer
	lastTouched time.Time
}

func newEventHub() *eventHub {
	return &eventHub{
		clients: make(map[chan []byte]struct{}),
	}
}

func (h *eventHub) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *eventHub) publish(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func defaultInstrumentConfig(addr string, minAmp float64) instrumentConfig {
	return instrumentConfig{
		Title: "spank instrument lab",
		Web: instrumentWebConfig{
			Title: "spank instrument",
			Addr:  addr,
		},
		Input: instrumentInputConfig{
			MinAmplitude: minAmp,
		},
		Audio: instrumentAudioConfig{
			SamplesEnabled: true,
			SamplesGain:    1.0,
			MasterGain:     1.0,
			SynthGain:      1.0,
			RecordingsDir:  "instrument_recordings",
			FX: instrumentFXConfig{
				Delay: instrumentDelayConfig{
					Enabled:  true,
					TimeMs:   280,
					Feedback: 0.42,
					Mix:      0.30,
				},
				Reverb: instrumentReverbConfig{
					Enabled:    true,
					PreDelayMs: 36,
					Decay:      0.52,
					Mix:        0.24,
				},
			},
			Synth: instrumentSynthConfig{
				Enabled:     true,
				Family:      "synth",
				Wave:        "sine",
				Frequency:   164,
				PitchFollow: 0.9,
				DurationMs:  320,
				AttackMs:    6,
				ReleaseMs:   220,
				Gain:        0.55,
			},
			Layers: []instrumentLayerConfig{
				{ID: "pain-soft", Label: "Pain Soft", Pack: "pain", Trigger: "soft", Family: "pain", Gain: 0.85, Enabled: true, DelaySend: 0.18, ReverbSend: 0.32},
				{ID: "sexy-main", Label: "Sexy Main", Pack: "sexy", Trigger: "always", Family: "kinky", Gain: 0.95, Enabled: true, DelaySend: 0.28, ReverbSend: 0.36},
				{ID: "lizard-combo", Label: "Lizard Combo", Pack: "lizard", Trigger: "combo", Family: "combo", Gain: 0.60, Enabled: true, DelaySend: 0.34, ReverbSend: 0.28},
				{ID: "halo-impact", Label: "Halo Impact", Pack: "halo", Trigger: "hard", Family: "impact", Gain: 0.45, Enabled: false, DelaySend: 0.20, ReverbSend: 0.18},
			},
		},
		Visual: instrumentVisualConfig{
			BasePalette:  []string{"#07111f", "#14263c"},
			SoftPalette:  []string{"#2fd3ff", "#71ffc7"},
			HardPalette:  []string{"#ff6b57", "#ffd166"},
			ComboPalette: []string{"#ff4fd8", "#8b5cff"},
			Families: map[string][]string{
				"pain":      {"#86f0ff", "#4cc9f0"},
				"kinky":     {"#ff74a8", "#ff9770"},
				"impact":    {"#ffb703", "#fb5607"},
				"combo":     {"#d16bff", "#8338ec"},
				"recording": {"#ffffff", "#bbbbbb"},
			},
			Impact: instrumentImpact{
				FlashMs:            150,
				FadeMs:             900,
				ColorRGB:           []int{255, 255, 255},
				FullScaleAmplitude: 0.35,
				RippleScale:        1.35,
				Shake:              0.25,
				Bloom:              0.35,
			},
			Combo: instrumentCombo{
				ResetMs:    1500,
				DecayMs:    2200,
				Milestones: []int{2, 5, 8, 12},
				Labels:     []string{"tease", "build", "frenzy", "overload"},
			},
		},
	}
}

func normalizeInstrumentConfig(cfg *instrumentConfig, addr string, minAmp float64) {
	def := defaultInstrumentConfig(addr, minAmp)

	if strings.TrimSpace(cfg.Title) == "" {
		cfg.Title = def.Title
	}
	if strings.TrimSpace(cfg.Web.Title) == "" {
		cfg.Web.Title = def.Web.Title
	}
	if strings.TrimSpace(cfg.Web.Addr) == "" {
		cfg.Web.Addr = def.Web.Addr
	}
	if cfg.Input.MinAmplitude <= 0 || cfg.Input.MinAmplitude > 1 {
		cfg.Input.MinAmplitude = def.Input.MinAmplitude
	}
	if cfg.Audio.MasterGain <= 0 {
		cfg.Audio.MasterGain = def.Audio.MasterGain
	}
	if cfg.Audio.SamplesGain <= 0 {
		cfg.Audio.SamplesGain = def.Audio.SamplesGain
	}
	if cfg.Audio.SynthGain <= 0 {
		cfg.Audio.SynthGain = def.Audio.SynthGain
	}
	if strings.TrimSpace(cfg.Audio.RecordingsDir) == "" {
		cfg.Audio.RecordingsDir = def.Audio.RecordingsDir
	}
	if cfg.Audio.FX.Delay.TimeMs <= 0 {
		cfg.Audio.FX.Delay = def.Audio.FX.Delay
	}
	if cfg.Audio.FX.Reverb.PreDelayMs <= 0 {
		cfg.Audio.FX.Reverb = def.Audio.FX.Reverb
	}
	if strings.TrimSpace(cfg.Audio.Synth.Wave) == "" {
		cfg.Audio.Synth = def.Audio.Synth
	}
	if len(cfg.Audio.Layers) == 0 {
		cfg.Audio.Layers = def.Audio.Layers
	}
	if len(cfg.Visual.BasePalette) == 0 {
		cfg.Visual.BasePalette = def.Visual.BasePalette
	}
	if len(cfg.Visual.SoftPalette) == 0 {
		cfg.Visual.SoftPalette = def.Visual.SoftPalette
	}
	if len(cfg.Visual.HardPalette) == 0 {
		cfg.Visual.HardPalette = def.Visual.HardPalette
	}
	if len(cfg.Visual.ComboPalette) == 0 {
		cfg.Visual.ComboPalette = def.Visual.ComboPalette
	}
	if len(cfg.Visual.Families) == 0 {
		cfg.Visual.Families = def.Visual.Families
	}
	if cfg.Visual.Impact.FlashMs <= 0 {
		cfg.Visual.Impact.FlashMs = def.Visual.Impact.FlashMs
	}
	if cfg.Visual.Impact.FadeMs <= 0 {
		cfg.Visual.Impact.FadeMs = def.Visual.Impact.FadeMs
	}
	cfg.Visual.Impact.FlashMs = 20
	cfg.Visual.Impact.FullScaleAmplitude = 0.05
	if len(cfg.Visual.Impact.ColorRGB) != 3 {
		cfg.Visual.Impact.ColorRGB = slices.Clone(def.Visual.Impact.ColorRGB)
	}
	if cfg.Visual.Impact.RippleScale <= 0 {
		cfg.Visual.Impact.RippleScale = def.Visual.Impact.RippleScale
	}
	if cfg.Visual.Impact.Shake <= 0 {
		cfg.Visual.Impact.Shake = def.Visual.Impact.Shake
	}
	if cfg.Visual.Impact.Bloom <= 0 {
		cfg.Visual.Impact.Bloom = def.Visual.Impact.Bloom
	}
	if cfg.Visual.Combo.ResetMs <= 0 {
		cfg.Visual.Combo.ResetMs = def.Visual.Combo.ResetMs
	}
	if cfg.Visual.Combo.DecayMs <= 0 {
		cfg.Visual.Combo.DecayMs = def.Visual.Combo.DecayMs
	}
	if len(cfg.Visual.Combo.Milestones) == 0 {
		cfg.Visual.Combo.Milestones = def.Visual.Combo.Milestones
	}
	if len(cfg.Visual.Combo.Labels) == 0 {
		cfg.Visual.Combo.Labels = def.Visual.Combo.Labels
	}

	for i := range cfg.Audio.Layers {
		layer := &cfg.Audio.Layers[i]
		if strings.TrimSpace(layer.ID) == "" {
			layer.ID = fmt.Sprintf("layer-%d", i+1)
		}
		if strings.TrimSpace(layer.Label) == "" {
			layer.Label = strings.ReplaceAll(layer.ID, "-", " ")
		}
		if strings.TrimSpace(layer.Trigger) == "" {
			layer.Trigger = "always"
		}
		if strings.TrimSpace(layer.Family) == "" {
			layer.Family = "impact"
		}
		if layer.Gain <= 0 {
			layer.Gain = 0.8
		}
		if layer.ID == "sexy-main" && layer.Pack == "sexy" && strings.EqualFold(strings.TrimSpace(layer.Trigger), "medium") {
			layer.Trigger = "always"
		}
	}

	if strings.TrimSpace(cfg.Audio.Synth.Family) == "" {
		cfg.Audio.Synth.Family = def.Audio.Synth.Family
	}
	if cfg.Audio.Synth.Frequency <= 0 {
		cfg.Audio.Synth.Frequency = def.Audio.Synth.Frequency
	}
	if cfg.Audio.Synth.PitchFollow < 0 {
		cfg.Audio.Synth.PitchFollow = def.Audio.Synth.PitchFollow
	}
	if cfg.Audio.Synth.DurationMs <= 0 {
		cfg.Audio.Synth.DurationMs = def.Audio.Synth.DurationMs
	}
	if cfg.Audio.Synth.AttackMs < 0 {
		cfg.Audio.Synth.AttackMs = def.Audio.Synth.AttackMs
	}
	if cfg.Audio.Synth.ReleaseMs < 0 {
		cfg.Audio.Synth.ReleaseMs = def.Audio.Synth.ReleaseMs
	}
	if cfg.Audio.Synth.Gain <= 0 {
		cfg.Audio.Synth.Gain = def.Audio.Synth.Gain
	}
	for i := range cfg.Visual.Impact.ColorRGB {
		cfg.Visual.Impact.ColorRGB[i] = max(0, min(255, cfg.Visual.Impact.ColorRGB[i]))
	}
}

func loadInstrumentConfig(path, addr string, minAmp float64) (instrumentConfig, error) {
	cfg := defaultInstrumentConfig(addr, minAmp)
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return instrumentConfig{}, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return instrumentConfig{}, err
	}
	normalizeInstrumentConfig(&cfg, addr, minAmp)
	return cfg, nil
}

func resolveInstrumentSavePath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "instrument.session.yaml"
	}
	return path
}

func resolveConfigDir(savePath string) string {
	base := filepath.Dir(resolveInstrumentSavePath(savePath))
	if abs, err := filepath.Abs(base); err == nil {
		return abs
	}
	return filepath.Clean(base)
}

func resolveRecordingDir(baseDir, recordingsDir string) string {
	if filepath.IsAbs(recordingsDir) {
		return filepath.Clean(recordingsDir)
	}
	return filepath.Clean(filepath.Join(baseDir, recordingsDir))
}

func resolveLayerPath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}

func rebaseLayerPath(path, oldBaseDir, newBaseDir string) string {
	if strings.TrimSpace(path) == "" || filepath.IsAbs(path) {
		return path
	}
	absPath := resolveLayerPath(oldBaseDir, path)
	relPath, err := filepath.Rel(newBaseDir, absPath)
	if err != nil {
		return absPath
	}
	return filepath.Clean(relPath)
}

func sanitizeRecordingName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return ""
	}
	return out
}

func isManagedRecordingPath(recordingsDir, candidate string) bool {
	if strings.TrimSpace(candidate) == "" {
		return false
	}
	rel, err := filepath.Rel(recordingsDir, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func isManagedRecordingLayer(baseDir, recordingsDir string, layerCfg instrumentLayerConfig) bool {
	if strings.TrimSpace(layerCfg.Path) == "" {
		return false
	}
	return isManagedRecordingPath(resolveRecordingDir(baseDir, recordingsDir), resolveLayerPath(baseDir, layerCfg.Path))
}

func newInstrumentState(cfg instrumentConfig, cooldown time.Duration, savePath string) (*instrumentState, error) {
	state := &instrumentState{
		cfg:       cfg,
		layers:    make(map[string]*instrumentLayer, len(cfg.Audio.Layers)),
		hub:       newEventHub(),
		audio:     newInstrumentAudioEngine(),
		savePath:  resolveInstrumentSavePath(savePath),
		configDir: resolveConfigDir(savePath),
	}
	for _, layerCfg := range cfg.Audio.Layers {
		layerCfg.Managed = isManagedRecordingLayer(state.configDir, cfg.Audio.RecordingsDir, layerCfg)
		pack, resolvedPath, err := soundPackFromLayer(layerCfg, state.configDir)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", layerCfg.ID, err)
		}
		if len(pack.files) == 0 {
			if err := pack.loadFiles(); err != nil {
				return nil, fmt.Errorf("layer %s: %w", layerCfg.ID, err)
			}
		} else {
			sort.Strings(pack.files)
		}
		layer := &instrumentLayer{
			cfg:          layerCfg,
			pack:         pack,
			tracker:      newSlapTracker(pack, cooldown),
			resolvedPath: resolvedPath,
		}
		state.layers[layerCfg.ID] = layer
	}
	for i := range state.cfg.Audio.Layers {
		state.cfg.Audio.Layers[i].Managed = isManagedRecordingLayer(state.configDir, cfg.Audio.RecordingsDir, state.cfg.Audio.Layers[i])
	}

	return state, nil
}

func soundPackFromLayer(cfg instrumentLayerConfig, baseDir string) (*soundPack, string, error) {
	if cfg.Pack != "" {
		switch cfg.Pack {
		case "pain":
			return &soundPack{name: "pain", fs: painAudio, dir: "audio/pain", mode: modeRandom}, "", nil
		case "sexy":
			return &soundPack{name: "sexy", fs: sexyAudio, dir: "audio/sexy", mode: modeEscalation}, "", nil
		case "halo":
			return &soundPack{name: "halo", fs: haloAudio, dir: "audio/halo", mode: modeRandom}, "", nil
		case "lizard":
			return &soundPack{name: "lizard", fs: lizardAudio, dir: "audio/lizard", mode: modeEscalation}, "", nil
		default:
			return nil, "", fmt.Errorf("unknown built-in pack %q", cfg.Pack)
		}
	}

	if cfg.Path == "" {
		return nil, "", fmt.Errorf("layer must define either pack or path")
	}

	resolvedPath := resolveLayerPath(baseDir, cfg.Path)
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, "", err
	}
	if info.IsDir() {
		return &soundPack{name: cfg.ID, dir: resolvedPath, mode: modeRandom, custom: true}, resolvedPath, nil
	}
	if !isSupportedAudioFile(resolvedPath) {
		return nil, "", fmt.Errorf("unsupported audio format %q", cfg.Path)
	}
	return &soundPack{name: cfg.ID, mode: modeRandom, custom: true, files: []string{resolvedPath}}, resolvedPath, nil
}

func (st *instrumentState) addRecordingLayer(label, path string) (instrumentLayerConfig, error) {
	if strings.TrimSpace(label) == "" {
		label = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	storedPath := path
	if !filepath.IsAbs(st.cfg.Audio.RecordingsDir) {
		storedPath = filepath.Clean(filepath.Join(st.cfg.Audio.RecordingsDir, filepath.Base(path)))
	}
	layerCfg := instrumentLayerConfig{
		ID:         fmt.Sprintf("rec-%d", time.Now().UnixNano()),
		Label:      label,
		Path:       storedPath,
		Managed:    true,
		Trigger:    "always",
		Family:     "recording",
		Gain:       1.0,
		Enabled:    true,
		Loop:       false,
		DelaySend:  0.10,
		ReverbSend: 0.18,
	}
	pack, resolvedPath, err := soundPackFromLayer(layerCfg, st.configDir)
	if err != nil {
		return instrumentLayerConfig{}, err
	}
	layer := &instrumentLayer{
		cfg:          layerCfg,
		pack:         pack,
		tracker:      newSlapTracker(pack, time.Duration(cooldownMs)*time.Millisecond),
		resolvedPath: resolvedPath,
	}
	st.layers[layerCfg.ID] = layer
	st.cfg.Audio.Layers = append(st.cfg.Audio.Layers, layerCfg)
	return layerCfg, nil
}

func (st *instrumentState) createRecording(name string, data []byte) (instrumentRecordingResponse, error) {
	st.mu.Lock()
	recordingsDir := resolveRecordingDir(st.configDir, st.cfg.Audio.RecordingsDir)
	st.mu.Unlock()

	if err := os.MkdirAll(recordingsDir, 0o755); err != nil {
		return instrumentRecordingResponse{}, err
	}

	baseName := sanitizeRecordingName(name)
	if baseName == "" {
		baseName = time.Now().Format("20060102-150405")
	}
	baseName = fmt.Sprintf("%s-%04d", baseName, time.Now().UnixNano()%10000)
	filePath := filepath.Join(recordingsDir, baseName+".wav")
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return instrumentRecordingResponse{}, err
	}

	st.mu.Lock()
	layerCfg, err := st.addRecordingLayer(strings.TrimSpace(name), filePath)
	if err != nil {
		st.mu.Unlock()
		_ = os.Remove(filePath)
		return instrumentRecordingResponse{}, err
	}
	if layerCfg.Label == "" {
		layerCfg.Label = strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	}
	for i := range st.cfg.Audio.Layers {
		if st.cfg.Audio.Layers[i].ID == layerCfg.ID {
			st.cfg.Audio.Layers[i].Label = layerCfg.Label
			break
		}
	}
	st.markDirtyLocked()
	st.mu.Unlock()

	return instrumentRecordingResponse{
		Snapshot: st.snapshot(),
		LayerID:  layerCfg.ID,
		Path:     layerCfg.Path,
	}, nil
}

func (st *instrumentState) deleteManagedLayer(id string) (instrumentSnapshot, error) {
	st.mu.Lock()
	layer, ok := st.layers[id]
	if !ok {
		st.mu.Unlock()
		return instrumentSnapshot{}, fmt.Errorf("unknown layer %q", id)
	}
	if !layer.cfg.Managed || strings.TrimSpace(layer.resolvedPath) == "" {
		st.mu.Unlock()
		return instrumentSnapshot{}, fmt.Errorf("layer %q is not a managed recording", id)
	}
	delete(st.layers, id)
	for i := range st.cfg.Audio.Layers {
		if st.cfg.Audio.Layers[i].ID == id {
			st.cfg.Audio.Layers = append(st.cfg.Audio.Layers[:i], st.cfg.Audio.Layers[i+1:]...)
			break
		}
	}
	st.markDirtyLocked()
	layerPath := layer.resolvedPath
	st.mu.Unlock()

	st.audio.stopLoop(id)
	if err := os.Remove(layerPath); err != nil && !os.IsNotExist(err) {
		return st.snapshot(), err
	}
	return st.snapshot(), nil
}

func (st *instrumentState) snapshot() instrumentSnapshot {
	st.mu.RLock()
	defer st.mu.RUnlock()

	return instrumentSnapshot{
		Config: st.cfg,
		Runtime: instrumentRuntimeState{
			Combo:       st.combo,
			Energy:      st.energy,
			Tier:        st.comboTier(st.combo),
			LastHitUnix: st.lastHit.UnixMilli(),
			ActiveLoops: st.audio.activeLoops(),
		},
		Meta: instrumentMeta{
			SavePath:      st.savePath,
			Dirty:         st.dirty,
			LastSavedUnix: st.lastSaved.UnixMilli(),
			LastSaveError: st.lastSaveError,
		},
	}
}

func (st *instrumentState) comboTier(combo int) string {
	labels := st.cfg.Visual.Combo.Labels
	milestones := st.cfg.Visual.Combo.Milestones

	tier := "idle"
	for i, milestone := range milestones {
		if combo < milestone {
			break
		}
		if i < len(labels) {
			tier = labels[i]
		}
	}
	return tier
}

func (st *instrumentState) markDirtyLocked() {
	st.dirty = true
	st.lastSaveError = ""
}

func (st *instrumentState) updateMixer(patch instrumentMixerPatch) instrumentSnapshot {
	st.mu.Lock()
	if patch.SamplesEnabled != nil {
		st.cfg.Audio.SamplesEnabled = *patch.SamplesEnabled
		st.markDirtyLocked()
	}
	if patch.SamplesGain != nil {
		st.cfg.Audio.SamplesGain = max(0.0, *patch.SamplesGain)
		st.markDirtyLocked()
	}
	if patch.MasterGain != nil {
		st.cfg.Audio.MasterGain = max(0.01, *patch.MasterGain)
		st.markDirtyLocked()
	}
	if patch.SynthGain != nil {
		st.cfg.Audio.SynthGain = max(0.0, *patch.SynthGain)
		st.markDirtyLocked()
	}
	st.mu.Unlock()
	return st.snapshot()
}

func (st *instrumentState) updateInput(patch instrumentInputPatch) instrumentSnapshot {
	st.mu.Lock()
	if patch.MinAmplitude != nil {
		st.cfg.Input.MinAmplitude = min(1.0, max(0.01, *patch.MinAmplitude))
		st.markDirtyLocked()
	}
	st.mu.Unlock()
	return st.snapshot()
}

func (st *instrumentState) updateLayer(id string, patch instrumentLayerPatch) (instrumentSnapshot, error) {
	st.mu.Lock()
	layer, ok := st.layers[id]
	if !ok {
		st.mu.Unlock()
		return instrumentSnapshot{}, fmt.Errorf("unknown layer %q", id)
	}

	if patch.Gain != nil {
		layer.cfg.Gain = max(0.0, *patch.Gain)
		st.markDirtyLocked()
	}
	if patch.Enabled != nil {
		layer.cfg.Enabled = *patch.Enabled
		st.markDirtyLocked()
	}
	if patch.DelaySend != nil {
		layer.cfg.DelaySend = min(1.0, max(0.0, *patch.DelaySend))
		st.markDirtyLocked()
	}
	if patch.ReverbSend != nil {
		layer.cfg.ReverbSend = min(1.0, max(0.0, *patch.ReverbSend))
		st.markDirtyLocked()
	}

	for i := range st.cfg.Audio.Layers {
		if st.cfg.Audio.Layers[i].ID != id {
			continue
		}
		st.cfg.Audio.Layers[i].Gain = layer.cfg.Gain
		st.cfg.Audio.Layers[i].Enabled = layer.cfg.Enabled
		st.cfg.Audio.Layers[i].DelaySend = layer.cfg.DelaySend
		st.cfg.Audio.Layers[i].ReverbSend = layer.cfg.ReverbSend
		break
	}
	st.mu.Unlock()
	return st.snapshot(), nil
}

func (st *instrumentState) updateFX(patch instrumentFXPatch) instrumentSnapshot {
	st.mu.Lock()
	if patch.DelayTimeMs != nil {
		st.cfg.Audio.FX.Delay.TimeMs = max(10, *patch.DelayTimeMs)
		st.markDirtyLocked()
	}
	if patch.DelayFeedback != nil {
		st.cfg.Audio.FX.Delay.Feedback = min(0.999, max(0.0, *patch.DelayFeedback))
		st.markDirtyLocked()
	}
	if patch.DelayMix != nil {
		st.cfg.Audio.FX.Delay.Mix = min(1.0, max(0.0, *patch.DelayMix))
		st.markDirtyLocked()
	}
	if patch.DelayEnabled != nil {
		st.cfg.Audio.FX.Delay.Enabled = *patch.DelayEnabled
		st.markDirtyLocked()
	}
	if patch.ReverbPreDelay != nil {
		st.cfg.Audio.FX.Reverb.PreDelayMs = max(0, *patch.ReverbPreDelay)
		st.markDirtyLocked()
	}
	if patch.ReverbDecay != nil {
		st.cfg.Audio.FX.Reverb.Decay = min(0.999, max(0.0, *patch.ReverbDecay))
		st.markDirtyLocked()
	}
	if patch.ReverbMix != nil {
		st.cfg.Audio.FX.Reverb.Mix = min(1.0, max(0.0, *patch.ReverbMix))
		st.markDirtyLocked()
	}
	if patch.ReverbEnabled != nil {
		st.cfg.Audio.FX.Reverb.Enabled = *patch.ReverbEnabled
		st.markDirtyLocked()
	}
	st.mu.Unlock()
	return st.snapshot()
}

func (st *instrumentState) updateSynth(patch instrumentSynthPatch) instrumentSnapshot {
	st.mu.Lock()
	if patch.Enabled != nil {
		st.cfg.Audio.Synth.Enabled = *patch.Enabled
		st.markDirtyLocked()
	}
	if patch.Wave != nil {
		st.cfg.Audio.Synth.Wave = strings.TrimSpace(*patch.Wave)
		st.markDirtyLocked()
	}
	if patch.Frequency != nil {
		st.cfg.Audio.Synth.Frequency = max(20.0, *patch.Frequency)
		st.markDirtyLocked()
	}
	if patch.PitchFollow != nil {
		st.cfg.Audio.Synth.PitchFollow = max(0.0, *patch.PitchFollow)
		st.markDirtyLocked()
	}
	if patch.DurationMs != nil {
		st.cfg.Audio.Synth.DurationMs = max(20, *patch.DurationMs)
		st.markDirtyLocked()
	}
	if patch.AttackMs != nil {
		st.cfg.Audio.Synth.AttackMs = max(0, *patch.AttackMs)
		st.markDirtyLocked()
	}
	if patch.ReleaseMs != nil {
		st.cfg.Audio.Synth.ReleaseMs = max(0, *patch.ReleaseMs)
		st.markDirtyLocked()
	}
	if patch.Gain != nil {
		st.cfg.Audio.Synth.Gain = max(0.0, *patch.Gain)
		st.markDirtyLocked()
	}
	st.mu.Unlock()
	return st.snapshot()
}

func (st *instrumentState) updateVisual(patch instrumentVisualPatch) instrumentSnapshot {
	st.mu.Lock()
	if len(patch.ColorRGB) == 3 {
		st.cfg.Visual.Impact.ColorRGB = make([]int, 3)
		for i := range patch.ColorRGB {
			st.cfg.Visual.Impact.ColorRGB[i] = max(0, min(255, patch.ColorRGB[i]))
		}
		st.markDirtyLocked()
	}
	if patch.FlashMs != nil {
		st.cfg.Visual.Impact.FlashMs = max(20, *patch.FlashMs)
		st.markDirtyLocked()
	}
	if patch.FadeMs != nil {
		st.cfg.Visual.Impact.FadeMs = max(60, *patch.FadeMs)
		st.markDirtyLocked()
	}
	if patch.FullScaleAmplitude != nil {
		st.cfg.Visual.Impact.FullScaleAmplitude = max(0.01, *patch.FullScaleAmplitude)
		st.markDirtyLocked()
	}
	st.mu.Unlock()
	return st.snapshot()
}

func (st *instrumentState) handleImpact(now time.Time, amplitude float64, severity string) {
	st.mu.Lock()
	elapsed := now.Sub(st.lastHit)
	if st.lastHit.IsZero() || elapsed > time.Duration(st.cfg.Visual.Combo.ResetMs)*time.Millisecond {
		st.combo = 1
		st.energy = amplitude
	} else {
		decay := float64(st.cfg.Visual.Combo.DecayMs) / 1000.0
		if decay > 0 {
			st.energy *= math.Pow(0.5, elapsed.Seconds()/decay)
		}
		st.combo++
		st.energy += amplitude
	}
	st.lastHit = now

	combo := st.combo
	energy := st.energy
	tier := st.comboTier(combo)
	masterGain := st.cfg.Audio.MasterGain
	samplesGain := st.cfg.Audio.SamplesGain
	synthGain := st.cfg.Audio.SynthGain
	samplesEnabled := st.cfg.Audio.SamplesEnabled
	fx := st.cfg.Audio.FX
	synth := st.cfg.Audio.Synth
	playbacks := make([]instrumentPlayback, 0, len(st.layers))
	layers := make([]*instrumentLayer, 0, len(st.layers))
	if samplesEnabled {
		for _, layer := range st.layers {
			if !layer.cfg.Enabled || !matchLayerTrigger(layer.cfg.Trigger, amplitude, combo) {
				continue
			}
			layers = append(layers, layer)
		}
	}
	st.mu.Unlock()

	for _, layer := range layers {
		_, score := layer.tracker.record(now)
		file := layer.tracker.getFile(score)
		gain := layer.cfg.Gain * samplesGain * masterGain
		playbacks = append(playbacks, instrumentPlayback{
			ID:      layer.cfg.ID,
			Label:   layer.cfg.Label,
			File:    file,
			Family:  layer.cfg.Family,
			Gain:    gain,
			Trigger: layer.cfg.Trigger,
		})
		if layer.cfg.Loop {
			if err := st.audio.startLoop(layer.cfg.ID, layer.pack, file, amplitude, gain, layer.cfg.DelaySend, layer.cfg.ReverbSend, fx, now); err != nil {
				fmt.Fprintf(os.Stderr, "spank: loop %s: %v\n", layer.cfg.ID, err)
			}
			continue
		}
		if err := st.audio.playSample(layer.pack, file, amplitude, gain, layer.cfg.DelaySend, layer.cfg.ReverbSend, fx); err != nil {
			fmt.Fprintf(os.Stderr, "spank: play %s: %v\n", file, err)
		}
	}

	if synth.Enabled {
		synthPlaybackGain := synth.Gain * synthGain * masterGain
		playbacks = append(playbacks, instrumentPlayback{
			ID:      "synth",
			Label:   "Synth",
			File:    synth.Wave,
			Family:  synth.Family,
			Gain:    synthPlaybackGain,
			Trigger: "enabled",
		})
		if err := st.audio.playSynth(synth, amplitude, synthGain*masterGain, fx); err != nil {
			fmt.Fprintf(os.Stderr, "spank: synth: %v\n", err)
		}
	}

	event := instrumentEvent{
		Timestamp:   now.Format(time.RFC3339Nano),
		Amplitude:   amplitude,
		Severity:    severity,
		Combo:       combo,
		Energy:      energy,
		Tier:        tier,
		ActiveLoops: st.audio.activeLoops(),
		Playbacks:   playbacks,
	}
	if data, err := json.Marshal(event); err == nil {
		st.hub.publish(data)
	}
}

func (st *instrumentState) tick(now time.Time) {
	st.mu.RLock()
	idleAfter := time.Duration(st.cfg.Visual.Combo.ResetMs) * time.Millisecond
	st.mu.RUnlock()
	st.audio.stopInactiveLoops(now, idleAfter)
}

func (st *instrumentState) shutdown() {
	st.audio.stopAllLoops()
}

func matchLayerTrigger(trigger string, amplitude float64, combo int) bool {
	switch strings.ToLower(strings.TrimSpace(trigger)) {
	case "", "always":
		return true
	case "soft":
		return amplitude < 0.14
	case "medium":
		return amplitude >= 0.14 && amplitude < 0.32
	case "hard":
		return amplitude >= 0.32
	case "combo":
		return combo >= 3
	default:
		return true
	}
}

const instrumentSampleRateHz = 44100

func newInstrumentAudioEngine() *instrumentAudioEngine {
	return &instrumentAudioEngine{
		loops: make(map[string]*instrumentLoopVoice),
	}
}

func (ae *instrumentAudioEngine) activeLoops() int {
	ae.mu.Lock()
	defer ae.mu.Unlock()
	return len(ae.loops)
}

func (ae *instrumentAudioEngine) stopLoop(id string) {
	ae.mu.Lock()
	defer ae.mu.Unlock()

	voice, ok := ae.loops[id]
	if !ok {
		return
	}
	speaker.Lock()
	voice.ctrl.Streamer = nil
	speaker.Unlock()
	closeClosers(voice.closers)
	delete(ae.loops, id)
}

func (ae *instrumentAudioEngine) ensureInit() {
	ae.mu.Lock()
	defer ae.mu.Unlock()
	if ae.initialized {
		return
	}

	sr := beep.SampleRate(instrumentSampleRateHz)
	speaker.Init(sr, sr.N(time.Second/20))
	ae.initialized = true
}

func (ae *instrumentAudioEngine) playSample(pack *soundPack, path string, amplitude, gain, delaySend, reverbSend float64, fx instrumentFXConfig) error {
	ae.ensureInit()

	stream, closers, err := buildSamplePlayback(pack, path, amplitude, gain, delaySend, reverbSend, fx, false)
	if err != nil {
		return err
	}

	speaker.Play(beep.Seq(stream, beep.Callback(func() {
		closeClosers(closers)
	})))
	return nil
}

func (ae *instrumentAudioEngine) startLoop(id string, pack *soundPack, path string, amplitude, gain, delaySend, reverbSend float64, fx instrumentFXConfig, now time.Time) error {
	ae.ensureInit()

	ae.mu.Lock()
	if voice, ok := ae.loops[id]; ok {
		voice.lastTouched = now
		ae.mu.Unlock()
		return nil
	}
	ae.mu.Unlock()

	stream, closers, err := buildSamplePlayback(pack, path, amplitude, gain, delaySend, reverbSend, fx, true)
	if err != nil {
		return err
	}

	ctrl := &beep.Ctrl{Streamer: stream}
	voice := &instrumentLoopVoice{
		ctrl:        ctrl,
		closers:     closers,
		lastTouched: now,
	}

	ae.mu.Lock()
	ae.loops[id] = voice
	ae.mu.Unlock()

	speaker.Play(ctrl)
	return nil
}

func (ae *instrumentAudioEngine) stopInactiveLoops(now time.Time, idleAfter time.Duration) {
	ae.mu.Lock()
	defer ae.mu.Unlock()

	for id, voice := range ae.loops {
		if now.Sub(voice.lastTouched) <= idleAfter {
			continue
		}
		speaker.Lock()
		voice.ctrl.Streamer = nil
		speaker.Unlock()
		closeClosers(voice.closers)
		delete(ae.loops, id)
	}
}

func (ae *instrumentAudioEngine) stopAllLoops() {
	ae.mu.Lock()
	defer ae.mu.Unlock()

	for id, voice := range ae.loops {
		speaker.Lock()
		voice.ctrl.Streamer = nil
		speaker.Unlock()
		closeClosers(voice.closers)
		delete(ae.loops, id)
	}
}

func (ae *instrumentAudioEngine) playSynth(cfg instrumentSynthConfig, amplitude, masterGain float64, fx instrumentFXConfig) error {
	ae.ensureInit()

	stream, err := buildSynthPlayback(cfg, amplitude, masterGain, fx)
	if err != nil {
		return err
	}

	speaker.Play(stream)
	return nil
}

func buildSamplePlayback(pack *soundPack, path string, amplitude, gain, delaySend, reverbSend float64, fx instrumentFXConfig, loop bool) (beep.Streamer, []io.Closer, error) {
	sr := beep.SampleRate(instrumentSampleRateHz)
	voices := make([]beep.Streamer, 0, 3)
	closers := make([]io.Closer, 0, 3)

	dry, dryCloser, err := openEngineAudioStreamer(pack, path, loop)
	if err != nil {
		return nil, nil, err
	}
	closers = append(closers, dryCloser)
	voices = append(voices, shapePlayback(dry, amplitude, gain))

	if fx.Delay.Enabled && fx.Delay.Mix > 0 && delaySend > 0 && fx.Delay.TimeMs > 0 {
		delaySource, delayCloser, err := openEngineAudioStreamer(pack, path, loop)
		if err != nil {
			closeClosers(closers)
			return nil, nil, err
		}
		closers = append(closers, delayCloser)
		delayWet := newFeedbackDelayStreamer(delaySource, sr.N(time.Duration(fx.Delay.TimeMs)*time.Millisecond), fx.Delay.Feedback)
		voices = append(voices, shapePlayback(delayWet, amplitude, gain*delaySend*fx.Delay.Mix))
	}

	if fx.Reverb.Enabled && fx.Reverb.Mix > 0 && reverbSend > 0 {
		reverbSource, reverbCloser, err := openEngineAudioStreamer(pack, path, loop)
		if err != nil {
			closeClosers(closers)
			return nil, nil, err
		}
		closers = append(closers, reverbCloser)
		if fx.Reverb.PreDelayMs > 0 {
			reverbSource = beep.Seq(beep.Silence(sr.N(time.Duration(fx.Reverb.PreDelayMs)*time.Millisecond)), reverbSource)
		}
		reverbWet := newSchroederReverbStreamer(reverbSource, fx.Reverb.Decay)
		voices = append(voices, shapePlayback(reverbWet, amplitude, gain*reverbSend*fx.Reverb.Mix))
	}

	return beep.Mix(voices...), closers, nil
}

func buildSynthPlayback(cfg instrumentSynthConfig, amplitude, masterGain float64, fx instrumentFXConfig) (beep.Streamer, error) {
	sr := beep.SampleRate(instrumentSampleRateHz)
	freq := cfg.Frequency * (1.0 + amplitude*cfg.PitchFollow)
	base, err := newSynthTone(sr, cfg.Wave, freq)
	if err != nil {
		return nil, err
	}

	total := sr.N(time.Duration(cfg.DurationMs) * time.Millisecond)
	attack := sr.N(time.Duration(cfg.AttackMs) * time.Millisecond)
	release := sr.N(time.Duration(cfg.ReleaseMs) * time.Millisecond)

	dry := shapePlayback(newEnvelopeStreamer(beep.Take(total, base), total, attack, release), amplitude, cfg.Gain*masterGain)
	voices := []beep.Streamer{dry}

	if fx.Delay.Enabled && fx.Delay.Mix > 0 && fx.Delay.TimeMs > 0 {
		tone, err := newSynthTone(sr, cfg.Wave, freq)
		if err != nil {
			return nil, err
		}
		delayInput := newEnvelopeStreamer(beep.Take(total, tone), total, attack, release)
		delayWet := newFeedbackDelayStreamer(delayInput, sr.N(time.Duration(fx.Delay.TimeMs)*time.Millisecond), fx.Delay.Feedback)
		voices = append(voices, shapePlayback(delayWet, amplitude, cfg.Gain*masterGain*fx.Delay.Mix))
	}

	if fx.Reverb.Enabled && fx.Reverb.Mix > 0 {
		tone, err := newSynthTone(sr, cfg.Wave, freq)
		if err != nil {
			return nil, err
		}
		var reverbInput beep.Streamer = newEnvelopeStreamer(beep.Take(total, tone), total, attack, release)
		if fx.Reverb.PreDelayMs > 0 {
			reverbInput = beep.Seq(beep.Silence(sr.N(time.Duration(fx.Reverb.PreDelayMs)*time.Millisecond)), reverbInput)
		}
		reverbWet := newSchroederReverbStreamer(reverbInput, fx.Reverb.Decay)
		voices = append(voices, shapePlayback(reverbWet, amplitude, cfg.Gain*masterGain*fx.Reverb.Mix))
	}

	return beep.Mix(voices...), nil
}

type feedbackDelayStreamer struct {
	input        beep.Streamer
	buffer       [][2]float64
	index        int
	feedback     float64
	inputDone    bool
	silentFrames int
}

func newFeedbackDelayStreamer(input beep.Streamer, delaySamples int, feedback float64) *feedbackDelayStreamer {
	if delaySamples < 1 {
		delaySamples = 1
	}
	return &feedbackDelayStreamer{
		input:    input,
		buffer:   make([][2]float64, delaySamples),
		feedback: min(0.999, max(0.0, feedback)),
	}
}

func (d *feedbackDelayStreamer) Stream(samples [][2]float64) (int, bool) {
	inBuf := make([][2]float64, len(samples))
	nIn := 0
	okIn := false
	if !d.inputDone {
		nIn, okIn = d.input.Stream(inBuf)
		if !okIn {
			d.inputDone = true
		}
	}

	peak := 0.0
	for i := range samples {
		in := [2]float64{}
		if i < nIn {
			in = inBuf[i]
		}
		delayed := d.buffer[d.index]
		d.buffer[d.index][0] = in[0] + delayed[0]*d.feedback
		d.buffer[d.index][1] = in[1] + delayed[1]*d.feedback
		samples[i] = delayed
		peak = max(peak, math.Abs(delayed[0]))
		peak = max(peak, math.Abs(delayed[1]))
		d.index = (d.index + 1) % len(d.buffer)
	}

	if d.inputDone {
		if peak < 1e-5 {
			d.silentFrames += len(samples)
		} else {
			d.silentFrames = 0
		}
		if d.silentFrames > len(d.buffer)*4 {
			return len(samples), false
		}
	}

	return len(samples), true
}

func (d *feedbackDelayStreamer) Err() error {
	return d.input.Err()
}

type combFilterMono struct {
	buffer    []float64
	index     int
	feedback  float64
	damping   float64
	filterMem float64
}

func newCombFilterMono(size int, feedback, damping float64) combFilterMono {
	return combFilterMono{
		buffer:   make([]float64, max(1, size)),
		feedback: feedback,
		damping:  damping,
	}
}

func (c *combFilterMono) process(input float64) float64 {
	out := c.buffer[c.index]
	c.filterMem = out*(1-c.damping) + c.filterMem*c.damping
	c.buffer[c.index] = input + c.filterMem*c.feedback
	c.index = (c.index + 1) % len(c.buffer)
	return out
}

type allpassFilterMono struct {
	buffer   []float64
	index    int
	feedback float64
}

func newAllpassFilterMono(size int, feedback float64) allpassFilterMono {
	return allpassFilterMono{
		buffer:   make([]float64, max(1, size)),
		feedback: feedback,
	}
}

func (a *allpassFilterMono) process(input float64) float64 {
	bufOut := a.buffer[a.index]
	out := -input + bufOut
	a.buffer[a.index] = input + bufOut*a.feedback
	a.index = (a.index + 1) % len(a.buffer)
	return out
}

type schroederReverbStreamer struct {
	input        beep.Streamer
	combsL       []combFilterMono
	combsR       []combFilterMono
	allpassL     []allpassFilterMono
	allpassR     []allpassFilterMono
	inputDone    bool
	silentFrames int
}

func newSchroederReverbStreamer(input beep.Streamer, decay float64) *schroederReverbStreamer {
	feedback := min(0.97, 0.35+decay*0.6)
	damping := 0.22
	return &schroederReverbStreamer{
		input: input,
		combsL: []combFilterMono{
			newCombFilterMono(1116, feedback, damping),
			newCombFilterMono(1188, feedback, damping),
			newCombFilterMono(1277, feedback, damping),
			newCombFilterMono(1356, feedback, damping),
			newCombFilterMono(1422, feedback, damping),
			newCombFilterMono(1491, feedback, damping),
		},
		combsR: []combFilterMono{
			newCombFilterMono(1139, feedback, damping),
			newCombFilterMono(1211, feedback, damping),
			newCombFilterMono(1300, feedback, damping),
			newCombFilterMono(1379, feedback, damping),
			newCombFilterMono(1445, feedback, damping),
			newCombFilterMono(1514, feedback, damping),
		},
		allpassL: []allpassFilterMono{
			newAllpassFilterMono(556, 0.5),
			newAllpassFilterMono(441, 0.5),
			newAllpassFilterMono(341, 0.5),
		},
		allpassR: []allpassFilterMono{
			newAllpassFilterMono(579, 0.5),
			newAllpassFilterMono(464, 0.5),
			newAllpassFilterMono(364, 0.5),
		},
	}
}

func (r *schroederReverbStreamer) Stream(samples [][2]float64) (int, bool) {
	inBuf := make([][2]float64, len(samples))
	nIn := 0
	okIn := false
	if !r.inputDone {
		nIn, okIn = r.input.Stream(inBuf)
		if !okIn {
			r.inputDone = true
		}
	}

	peak := 0.0
	for i := range samples {
		input := 0.0
		if i < nIn {
			input = (inBuf[i][0] + inBuf[i][1]) * 0.12
		}
		wetL := 0.0
		wetR := 0.0
		for j := range r.combsL {
			wetL += r.combsL[j].process(input)
			wetR += r.combsR[j].process(input)
		}
		for j := range r.allpassL {
			wetL = r.allpassL[j].process(wetL)
			wetR = r.allpassR[j].process(wetR)
		}
		wetL *= 0.22
		wetR *= 0.22
		samples[i][0] = wetL
		samples[i][1] = wetR
		peak = max(peak, math.Abs(wetL))
		peak = max(peak, math.Abs(wetR))
	}

	if r.inputDone {
		if peak < 1e-5 {
			r.silentFrames += len(samples)
		} else {
			r.silentFrames = 0
		}
		if r.silentFrames > instrumentSampleRateHz*4 {
			return len(samples), false
		}
	}

	return len(samples), true
}

func (r *schroederReverbStreamer) Err() error {
	return r.input.Err()
}

func openEngineAudioStreamer(pack *soundPack, path string, loop bool) (beep.Streamer, io.Closer, error) {
	streamer, format, err := openAudio(pack, path)
	if err != nil {
		return nil, nil, err
	}

	var source beep.Streamer
	if loop {
		source = beep.Loop(-1, streamer)
	} else {
		source = streamer
	}

	if format.SampleRate != beep.SampleRate(instrumentSampleRateHz) {
		source = beep.Resample(4, format.SampleRate, beep.SampleRate(instrumentSampleRateHz), source)
	}

	return source, streamer, nil
}

func closeClosers(closers []io.Closer) {
	for _, closer := range closers {
		if closer != nil {
			_ = closer.Close()
		}
	}
}

type envelopeStreamer struct {
	streamer beep.Streamer
	total    int
	attack   int
	release  int
	pos      int
}

func newEnvelopeStreamer(streamer beep.Streamer, total, attack, release int) *envelopeStreamer {
	return &envelopeStreamer{
		streamer: streamer,
		total:    max(1, total),
		attack:   attack,
		release:  release,
	}
}

func (e *envelopeStreamer) Stream(samples [][2]float64) (int, bool) {
	n, ok := e.streamer.Stream(samples)
	for i := range samples[:n] {
		env := 1.0
		if e.attack > 0 && e.pos < e.attack {
			env = float64(e.pos) / float64(e.attack)
		}
		if e.release > 0 && e.pos > e.total-e.release {
			remaining := max(0, e.total-e.pos)
			env = min(env, float64(remaining)/float64(e.release))
		}
		samples[i][0] *= env
		samples[i][1] *= env
		e.pos++
	}
	return n, ok
}

func (e *envelopeStreamer) Err() error {
	return e.streamer.Err()
}

func newSynthTone(sr beep.SampleRate, wave string, freq float64) (beep.Streamer, error) {
	switch strings.ToLower(strings.TrimSpace(wave)) {
	case "", "sine":
		return generators.SineTone(sr, freq)
	case "triangle":
		return generators.TriangleTone(sr, freq)
	case "square":
		return generators.SquareTone(sr, freq)
	case "saw":
		return generators.SawtoothTone(sr, freq)
	case "saw-rev", "reverse-saw":
		return generators.SawtoothToneReversed(sr, freq)
	default:
		return nil, fmt.Errorf("unsupported synth wave %q", wave)
	}
}

type slapTracker struct {
	mu       sync.Mutex
	score    float64
	lastTime time.Time
	total    int
	halfLife float64 // seconds
	scale    float64 // controls the escalation curve shape
	pack     *soundPack
}

func newSlapTracker(pack *soundPack, cooldown time.Duration) *slapTracker {
	// scale maps the exponential curve so that sustained max-rate
	// slapping (one per cooldown) reaches the final file. At steady
	// state the score converges to ssMax; we set scale so that score
	// maps to the last index.
	cooldownSec := cooldown.Seconds()
	ssMax := 1.0 / (1.0 - math.Pow(0.5, cooldownSec/decayHalfLife))
	scale := (ssMax - 1) / math.Log(float64(len(pack.files)+1))
	return &slapTracker{
		halfLife: decayHalfLife,
		scale:    scale,
		pack:     pack,
	}
}

func (st *slapTracker) record(now time.Time) (int, float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if !st.lastTime.IsZero() {
		elapsed := now.Sub(st.lastTime).Seconds()
		st.score *= math.Pow(0.5, elapsed/st.halfLife)
	}
	st.score += 1.0
	st.lastTime = now
	st.total++
	return st.total, st.score
}

func (st *slapTracker) getFile(score float64) string {
	if st.pack.mode == modeRandom {
		return st.pack.files[rand.Intn(len(st.pack.files))]
	}

	// Escalation: 1-exp(-x) curve maps score to file index.
	// At sustained max slap rate, score reaches ssMax which maps
	// to the final file.
	maxIdx := len(st.pack.files) - 1
	idx := min(int(float64(len(st.pack.files))*(1.0-math.Exp(-(score-1)/st.scale))), maxIdx)
	return st.pack.files[idx]
}

func main() {
	cmd := &cobra.Command{
		Use:   "spank",
		Short: "Yells 'ow!' when you slap the laptop",
		Long: `spank reads the Apple Silicon accelerometer directly via IOKit HID
and plays audio responses when a slap or hit is detected.

Requires sudo (for IOKit HID access to the accelerometer).

Use --sexy for a different experience. In sexy mode, the more you slap
within a minute, the more intense the sounds become.

Use --halo to play random audio clips from Halo soundtracks on each slap.

Use --lizard for lizard mode. Like sexy mode, the more you slap
within a minute, the more intense the sounds become.

Use --instrument to launch the browser-based instrument lab with
reactive visuals, combo HUD, layer volume controls, and YAML config.`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			tuning := defaultTuning()
			if fastMode {
				tuning = applyFastOverlay(tuning)
			}
			// Explicit flags override fast preset defaults
			if cmd.Flags().Changed("min-amplitude") {
				tuning.minAmplitude = minAmplitude
			}
			if cmd.Flags().Changed("cooldown") {
				tuning.cooldown = time.Duration(cooldownMs) * time.Millisecond
			}
			return run(cmd.Context(), tuning)
		},
		SilenceUsage: true,
	}

	cmd.Flags().BoolVarP(&sexyMode, "sexy", "s", false, "Enable sexy mode")
	cmd.Flags().BoolVarP(&haloMode, "halo", "H", false, "Enable halo mode")
	cmd.Flags().BoolVarP(&lizardMode, "lizard", "l", false, "Enable lizard mode (escalating intensity)")
	cmd.Flags().StringVarP(&customPath, "custom", "c", "", "Path to custom WAV/MP3 audio directory")
	cmd.Flags().BoolVar(&fastMode, "fast", false, "Enable faster detection tuning (shorter cooldown, higher sensitivity)")
	cmd.Flags().StringSliceVar(&customFiles, "custom-files", nil, "Comma-separated list of custom WAV/MP3 files")
	cmd.Flags().Float64Var(&minAmplitude, "min-amplitude", defaultMinAmplitude, "Minimum amplitude threshold (0.0-1.0, lower = more sensitive)")
	cmd.Flags().IntVar(&cooldownMs, "cooldown", defaultCooldownMs, "Cooldown between responses in milliseconds")
	cmd.Flags().BoolVar(&stdioMode, "stdio", false, "Enable stdio mode: JSON output and stdin commands (for GUI integration)")
	cmd.Flags().BoolVar(&volumeScaling, "volume-scaling", false, "Scale playback volume by slap amplitude (harder hits = louder)")
	cmd.Flags().Float64Var(&speedRatio, "speed", defaultSpeedRatio, "Playback speed multiplier (0.5 = half speed, 2.0 = double speed)")
	cmd.Flags().BoolVar(&instrumentMode, "instrument", false, "Launch instrument mode with a local web interface")
	cmd.Flags().StringVar(&configPath, "config", "", "Path to instrument YAML config")
	cmd.Flags().StringVar(&webAddr, "web-addr", "127.0.0.1:8765", "Address for the instrument web UI")

	if err := fang.Execute(context.Background(), cmd); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, tuning runtimeTuning) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("spank requires root privileges for accelerometer access, run with: sudo spank")
	}

	minAmplitude = tuning.minAmplitude
	cooldownMs = int(tuning.cooldown / time.Millisecond)

	modeCount := 0
	if sexyMode {
		modeCount++
	}
	if haloMode {
		modeCount++
	}
	if lizardMode {
		modeCount++
	}
	if customPath != "" || len(customFiles) > 0 {
		modeCount++
	}
	if instrumentMode && modeCount > 0 {
		return fmt.Errorf("--instrument cannot be combined with classic pack flags")
	}
	if modeCount > 1 {
		return fmt.Errorf("--sexy, --halo, --lizard, and --custom/--custom-files are mutually exclusive; pick one")
	}

	if tuning.minAmplitude < 0 || tuning.minAmplitude > 1 {
		return fmt.Errorf("--min-amplitude must be between 0.0 and 1.0")
	}
	if tuning.cooldown <= 0 {
		return fmt.Errorf("--cooldown must be greater than 0")
	}

	var (
		pack *soundPack
		inst *instrumentState
	)
	switch {
	case instrumentMode:
		cfg, err := loadInstrumentConfig(configPath, webAddr, tuning.minAmplitude)
		if err != nil {
			return fmt.Errorf("loading instrument config: %w", err)
		}
		inst, err = newInstrumentState(cfg, tuning.cooldown, configPath)
		if err != nil {
			return fmt.Errorf("building instrument state: %w", err)
		}
	case len(customFiles) > 0:
		for _, f := range customFiles {
			if !isSupportedAudioFile(f) {
				return fmt.Errorf("custom file must be WAV or MP3: %s", f)
			}
			if _, err := os.Stat(f); err != nil {
				return fmt.Errorf("custom file not found: %s", f)
			}
		}
		pack = &soundPack{name: "custom", mode: modeRandom, custom: true, files: customFiles}
	case customPath != "":
		pack = &soundPack{name: "custom", dir: customPath, mode: modeRandom, custom: true}
	case sexyMode:
		pack = &soundPack{name: "sexy", fs: sexyAudio, dir: "audio/sexy", mode: modeEscalation}
	case haloMode:
		pack = &soundPack{name: "halo", fs: haloAudio, dir: "audio/halo", mode: modeRandom}
	case lizardMode:
		pack = &soundPack{name: "lizard", fs: lizardAudio, dir: "audio/lizard", mode: modeEscalation}
	default:
		pack = &soundPack{name: "pain", fs: painAudio, dir: "audio/pain", mode: modeRandom}
	}

	if pack != nil && len(pack.files) == 0 {
		if err := pack.loadFiles(); err != nil {
			return fmt.Errorf("loading %s audio: %w", pack.name, err)
		}
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create shared memory for accelerometer data.
	accelRing, err := shm.CreateRing(shm.NameAccel)
	if err != nil {
		return fmt.Errorf("creating accel shm: %w", err)
	}
	defer accelRing.Close()
	defer accelRing.Unlink()

	// Start the sensor worker in a background goroutine.
	// sensor.Run() needs runtime.LockOSThread for CFRunLoop, which it
	// handles internally. We launch detection on the current goroutine.
	go func() {
		close(sensorReady)
		if err := sensor.Run(sensor.Config{
			AccelRing: accelRing,
			Restarts:  0,
		}); err != nil {
			sensorErr <- err
		}
	}()

	// Wait for sensor to be ready.
	select {
	case <-sensorReady:
	case err := <-sensorErr:
		return fmt.Errorf("sensor worker failed: %w", err)
	case <-ctx.Done():
		return nil
	}

	// Give the sensor a moment to start producing data.
	time.Sleep(sensorStartupDelay)

	if instrumentMode {
		server, err := startInstrumentServer(ctx, inst)
		if err != nil {
			return fmt.Errorf("starting instrument web UI: %w", err)
		}
		defer server.Close()
		defer inst.shutdown()
		fmt.Printf("spank: instrument lab running at http://%s\n", inst.cfg.Web.Addr)
		return listenForInstrument(ctx, accelRing, tuning, inst)
	}

	return listenForSlaps(ctx, pack, accelRing, tuning)
}

func startInstrumentServer(ctx context.Context, inst *instrumentState) (*http.Server, error) {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/events", inst.serveEvents)
	mux.HandleFunc("/api/state", inst.serveState)
	mux.HandleFunc("/api/input", inst.serveInputPatch)
	mux.HandleFunc("/api/mixer", inst.serveMixerPatch)
	mux.HandleFunc("/api/fx", inst.serveFXPatch)
	mux.HandleFunc("/api/synth", inst.serveSynthPatch)
	mux.HandleFunc("/api/synth/test", inst.serveSynthTest)
	mux.HandleFunc("/api/visual", inst.serveVisualPatch)
	mux.HandleFunc("/api/save", inst.serveSave)
	mux.HandleFunc("/api/recordings", inst.serveRecordingUpload)
	mux.HandleFunc("/api/layers/", inst.serveLayerPatch)

	server := &http.Server{
		Addr:    inst.cfg.Web.Addr,
		Handler: mux,
	}

	ln, err := net.Listen("tcp", inst.cfg.Web.Addr)
	if err != nil {
		return nil, err
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "spank: instrument server: %v\n", err)
		}
	}()

	return server, nil
}

func (st *instrumentState) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	if data, err := json.Marshal(st.snapshot()); err == nil {
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
		flusher.Flush()
	}

	ch := st.hub.subscribe()
	defer st.hub.unsubscribe(ch)

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: impact\ndata: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func (st *instrumentState) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, st.snapshot())
}

func (st *instrumentState) saveConfig(path string) (instrumentSnapshot, error) {
	st.mu.Lock()
	savePath := st.savePath
	if strings.TrimSpace(path) != "" {
		savePath = path
	}
	oldConfigDir := st.configDir
	cfg := st.cfg
	st.mu.Unlock()

	savePath = resolveInstrumentSavePath(savePath)
	newConfigDir := resolveConfigDir(savePath)
	if oldConfigDir != newConfigDir {
		for i := range cfg.Audio.Layers {
			if strings.TrimSpace(cfg.Audio.Layers[i].Path) == "" {
				continue
			}
			cfg.Audio.Layers[i].Path = rebaseLayerPath(cfg.Audio.Layers[i].Path, oldConfigDir, newConfigDir)
		}
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		st.mu.Lock()
		st.lastSaveError = err.Error()
		st.mu.Unlock()
		return st.snapshot(), err
	}

	dir := filepath.Dir(savePath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			st.mu.Lock()
			st.lastSaveError = err.Error()
			st.mu.Unlock()
			return st.snapshot(), err
		}
	}
	if err := os.WriteFile(savePath, data, 0o644); err != nil {
		st.mu.Lock()
		st.lastSaveError = err.Error()
		st.mu.Unlock()
		return st.snapshot(), err
	}

	st.mu.Lock()
	st.savePath = savePath
	st.configDir = newConfigDir
	st.cfg = cfg
	for id, layer := range st.layers {
		for i := range st.cfg.Audio.Layers {
			if st.cfg.Audio.Layers[i].ID != id {
				continue
			}
			layer.cfg.Path = st.cfg.Audio.Layers[i].Path
			break
		}
	}
	st.dirty = false
	st.lastSaved = time.Now()
	st.lastSaveError = ""
	st.mu.Unlock()
	return st.snapshot(), nil
}

func (st *instrumentState) serveSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req instrumentSaveRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	snapshot, err := st.saveConfig(strings.TrimSpace(req.Path))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, snapshot)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (st *instrumentState) serveMixerPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var patch instrumentMixerPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, st.updateMixer(patch))
}

func (st *instrumentState) serveInputPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var patch instrumentInputPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, st.updateInput(patch))
}

func (st *instrumentState) serveFXPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var patch instrumentFXPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, st.updateFX(patch))
}

func (st *instrumentState) serveSynthPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var patch instrumentSynthPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, st.updateSynth(patch))
}

func (st *instrumentState) serveVisualPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var patch instrumentVisualPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, st.updateVisual(patch))
}

func (st *instrumentState) serveSynthTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	st.mu.RLock()
	cfg := st.cfg.Audio.Synth
	masterGain := st.cfg.Audio.MasterGain
	fx := st.cfg.Audio.FX
	st.mu.RUnlock()

	if err := st.audio.playSynth(cfg, 0.55, masterGain, fx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, st.snapshot())
}

func (st *instrumentState) serveRecordingUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing wav file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 16<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		http.Error(w, "uploaded file is not a wav", http.StatusBadRequest)
		return
	}

	resp, err := st.createRecording(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (st *instrumentState) serveLayerPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		id := strings.TrimPrefix(r.URL.Path, "/api/layers/")
		if id == "" {
			http.Error(w, "missing layer id", http.StatusBadRequest)
			return
		}
		snapshot, err := st.deleteManagedLayer(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/layers/")
	if id == "" {
		http.Error(w, "missing layer id", http.StatusBadRequest)
		return
	}

	var patch instrumentLayerPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	snapshot, err := st.updateLayer(id, patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func listenForInstrument(ctx context.Context, accelRing *shm.RingBuffer, tuning runtimeTuning, inst *instrumentState) error {
	det := detector.New()
	var lastAccelTotal uint64
	var lastEventTime time.Time
	var lastTrigger time.Time

	fmt.Printf("spank: listening for slaps in instrument mode... (ctrl+c to quit)\n")

	ticker := time.NewTicker(tuning.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		case err := <-sensorErr:
			return fmt.Errorf("sensor worker failed: %w", err)
		case <-ticker.C:
		}

		inst.tick(time.Now())

		now := time.Now()
		tNow := float64(now.UnixNano()) / 1e9

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal
		if len(samples) > tuning.maxBatch {
			samples = samples[len(samples)-tuning.maxBatch:]
		}

		nSamples := len(samples)
		for idx, sample := range samples {
			tSample := tNow - float64(nSamples-idx-1)/float64(det.FS)
			det.Process(sample.X, sample.Y, sample.Z, tSample)
		}

		if len(det.Events) == 0 {
			continue
		}

		ev := det.Events[len(det.Events)-1]
		if ev.Time.Equal(lastEventTime) {
			continue
		}
		lastEventTime = ev.Time

		if time.Since(lastTrigger) <= tuning.cooldown {
			continue
		}
		inst.mu.RLock()
		threshold := inst.cfg.Input.MinAmplitude
		inst.mu.RUnlock()
		if ev.Amplitude < threshold {
			continue
		}

		lastTrigger = now
		inst.handleImpact(now, ev.Amplitude, string(ev.Severity))
	}
}

func listenForSlaps(ctx context.Context, pack *soundPack, accelRing *shm.RingBuffer, tuning runtimeTuning) error {
	tracker := newSlapTracker(pack, tuning.cooldown)
	speakerInit := false
	det := detector.New()
	var lastAccelTotal uint64
	var lastEventTime time.Time
	var lastYell time.Time

	// Start stdin command reader if in JSON mode
	if stdioMode {
		go readStdinCommands()
	}

	presetLabel := "default"
	if fastMode {
		presetLabel = "fast"
	}
	fmt.Printf("spank: listening for slaps in %s mode with %s tuning... (ctrl+c to quit)\n", pack.name, presetLabel)
	if stdioMode {
		fmt.Println(`{"status":"ready"}`)
	}

	ticker := time.NewTicker(tuning.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		case err := <-sensorErr:
			return fmt.Errorf("sensor worker failed: %w", err)
		case <-ticker.C:
		}

		// Check if paused
		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			continue
		}

		now := time.Now()
		tNow := float64(now.UnixNano()) / 1e9

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal
		if len(samples) > tuning.maxBatch {
			samples = samples[len(samples)-tuning.maxBatch:]
		}

		nSamples := len(samples)
		for idx, sample := range samples {
			tSample := tNow - float64(nSamples-idx-1)/float64(det.FS)
			det.Process(sample.X, sample.Y, sample.Z, tSample)
		}

		if len(det.Events) == 0 {
			continue
		}

		ev := det.Events[len(det.Events)-1]
		if ev.Time.Equal(lastEventTime) {
			continue
		}
		lastEventTime = ev.Time

		if time.Since(lastYell) <= time.Duration(cooldownMs)*time.Millisecond {
			continue
		}
		if ev.Amplitude < minAmplitude {
			continue
		}

		lastYell = now
		num, score := tracker.record(now)
		file := tracker.getFile(score)
		if stdioMode {
			event := map[string]interface{}{
				"timestamp":  now.Format(time.RFC3339Nano),
				"slapNumber": num,
				"amplitude":  ev.Amplitude,
				"severity":   string(ev.Severity),
				"file":       file,
			}
			if data, err := json.Marshal(event); err == nil {
				fmt.Println(string(data))
			}
		} else {
			fmt.Printf("slap #%d [%s amp=%.5fg] -> %s\n", num, ev.Severity, ev.Amplitude, file)
		}
		go playAudio(pack, file, ev.Amplitude, 1.0, &speakerInit)
	}
}

var speakerMu sync.Mutex

// amplitudeToVolume maps a detected amplitude to a beep/effects.Volume
// level. Amplitude typically ranges from ~0.05 (light tap) to ~1.0+
// (hard slap). The mapping uses a logarithmic curve so that light taps
// are noticeably quieter and hard hits play near full volume.
//
// Returns a value in the range [-3.0, 0.0] for use with effects.Volume
// (base 2): -3.0 is ~1/8 volume, 0.0 is full volume.
func amplitudeToVolume(amplitude float64) float64 {
	const (
		minAmp = 0.05 // softest detectable
		maxAmp = 0.80 // treat anything above this as max
		minVol = -3.0 // quietest playback (1/8 volume with base 2)
		maxVol = 0.0  // full volume
	)

	// Clamp
	if amplitude <= minAmp {
		return minVol
	}
	if amplitude >= maxAmp {
		return maxVol
	}

	// Normalize to [0, 1]
	t := (amplitude - minAmp) / (maxAmp - minAmp)

	// Log curve for more natural volume scaling
	// log(1 + t*99) / log(100) maps [0,1] -> [0,1] with a log curve
	t = math.Log(1+t*99) / math.Log(100)

	return minVol + t*(maxVol-minVol)
}

func linearGainToVolume(gain float64) float64 {
	if gain <= 0 {
		return -12.0
	}
	return math.Log2(gain)
}

func decodeAudio(path string, reader io.ReadCloser) (beep.StreamSeekCloser, beep.Format, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3":
		return mp3.Decode(reader)
	case ".wav":
		return wav.Decode(reader)
	default:
		return nil, beep.Format{}, fmt.Errorf("unsupported audio format: %s", path)
	}
}

func openAudio(pack *soundPack, path string) (beep.StreamSeekCloser, beep.Format, error) {
	if pack.custom {
		file, err := os.Open(path)
		if err != nil {
			return nil, beep.Format{}, err
		}
		streamer, format, err := decodeAudio(path, file)
		if err != nil {
			_ = file.Close()
			return nil, beep.Format{}, err
		}
		return streamer, format, nil
	}

	data, err := pack.fs.ReadFile(path)
	if err != nil {
		return nil, beep.Format{}, err
	}
	return decodeAudio(path, io.NopCloser(bytes.NewReader(data)))
}

func shapePlayback(source beep.Streamer, amplitude float64, gain float64) beep.Streamer {
	volume := linearGainToVolume(gain)
	if volumeScaling {
		volume += amplitudeToVolume(amplitude)
	}
	if volumeScaling || math.Abs(volume) > 0.001 {
		source = &effects.Volume{
			Streamer: source,
			Base:     2,
			Volume:   volume,
			Silent:   false,
		}
	}
	if speedRatio != 1.0 && speedRatio > 0 {
		source = beep.ResampleRatio(4, speedRatio, source)
	}
	return source
}

func playAudio(pack *soundPack, path string, amplitude float64, gain float64, speakerInit *bool) {
	streamer, format, err := openAudio(pack, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spank: open %s: %v\n", path, err)
		return
	}
	defer streamer.Close()

	speakerMu.Lock()
	if !*speakerInit {
		speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/20))
		*speakerInit = true
	}
	speakerMu.Unlock()

	source := shapePlayback(streamer, amplitude, gain)

	done := make(chan bool)
	speaker.Play(beep.Seq(source, beep.Callback(func() {
		done <- true
	})))
	<-done
}

// stdinCommand represents a command received via stdin
type stdinCommand struct {
	Cmd       string  `json:"cmd"`
	Amplitude float64 `json:"amplitude,omitempty"`
	Cooldown  int     `json:"cooldown,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
}

// readStdinCommands reads JSON commands from stdin for live control
func readStdinCommands() {
	processCommands(os.Stdin, os.Stdout)
}

// processCommands reads JSON commands from r and writes responses to w.
// This is the testable core of the stdin command handler.
func processCommands(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var cmd stdinCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			if stdioMode {
				fmt.Fprintf(w, `{"error":"invalid command: %s"}%s`, err.Error(), "\n")
			}
			continue
		}

		switch cmd.Cmd {
		case "pause":
			pausedMu.Lock()
			paused = true
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"paused"}`)
			}
		case "resume":
			pausedMu.Lock()
			paused = false
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"resumed"}`)
			}
		case "set":
			if cmd.Amplitude > 0 && cmd.Amplitude <= 1 {
				minAmplitude = cmd.Amplitude
			}
			if cmd.Cooldown > 0 {
				cooldownMs = cmd.Cooldown
			}
			if cmd.Speed > 0 {
				speedRatio = cmd.Speed
			}
			if stdioMode {
				fmt.Fprintf(w, `{"status":"settings_updated","amplitude":%.4f,"cooldown":%d,"speed":%.2f}%s`, minAmplitude, cooldownMs, speedRatio, "\n")
			}
		case "volume-scaling":
			volumeScaling = !volumeScaling
			if stdioMode {
				fmt.Fprintf(w, `{"status":"volume_scaling_toggled","volume_scaling":%t}%s`, volumeScaling, "\n")
			}
		case "status":
			pausedMu.RLock()
			isPaused := paused
			pausedMu.RUnlock()
			if stdioMode {
				fmt.Fprintf(w, `{"status":"ok","paused":%t,"amplitude":%.4f,"cooldown":%d,"volume_scaling":%t,"speed":%.2f}%s`, isPaused, minAmplitude, cooldownMs, volumeScaling, speedRatio, "\n")
			}
		default:
			if stdioMode {
				fmt.Fprintf(w, `{"error":"unknown command: %s"}%s`, cmd.Cmd, "\n")
			}
		}
	}
}

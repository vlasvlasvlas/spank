package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewInstrumentStateResolvesRelativeLayerPathFromConfig(t *testing.T) {
	baseDir := t.TempDir()
	assetDir := filepath.Join(baseDir, "assets")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	assetPath := filepath.Join(assetDir, "custom.wav")
	if err := os.WriteFile(assetPath, []byte("wav"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	cfg := defaultInstrumentConfig("127.0.0.1:8765", 0.05)
	cfg.Audio.Layers = []instrumentLayerConfig{
		{
			ID:      "custom",
			Label:   "Custom",
			Path:    "assets/custom.wav",
			Trigger: "always",
			Family:  "custom",
			Gain:    1.0,
			Enabled: true,
		},
	}

	state, err := newInstrumentState(cfg, 500*time.Millisecond, filepath.Join(baseDir, "preset.yaml"))
	if err != nil {
		t.Fatalf("newInstrumentState: %v", err)
	}

	layer := state.layers["custom"]
	if layer == nil {
		t.Fatal("expected runtime layer to be created")
	}
	if layer.resolvedPath != assetPath {
		t.Fatalf("resolved path mismatch: got %q want %q", layer.resolvedPath, assetPath)
	}
	if got := layer.pack.files[0]; got != assetPath {
		t.Fatalf("sound pack file mismatch: got %q want %q", got, assetPath)
	}
	if state.cfg.Audio.Layers[0].Path != "assets/custom.wav" {
		t.Fatalf("config path should remain relative, got %q", state.cfg.Audio.Layers[0].Path)
	}
}

func TestSaveConfigRebasesRelativeLayerPathsToNewYaml(t *testing.T) {
	rootDir := t.TempDir()
	oldDir := filepath.Join(rootDir, "old")
	newDir := filepath.Join(rootDir, "new")
	assetDir := filepath.Join(oldDir, "assets")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("mkdir asset dir: %v", err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir new dir: %v", err)
	}
	assetPath := filepath.Join(assetDir, "custom.wav")
	if err := os.WriteFile(assetPath, []byte("wav"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	cfg := defaultInstrumentConfig("127.0.0.1:8765", 0.05)
	cfg.Audio.Layers = []instrumentLayerConfig{
		{
			ID:      "custom",
			Label:   "Custom",
			Path:    "assets/custom.wav",
			Trigger: "always",
			Family:  "custom",
			Gain:    1.0,
			Enabled: true,
		},
	}

	state, err := newInstrumentState(cfg, time.Second, filepath.Join(oldDir, "preset.yaml"))
	if err != nil {
		t.Fatalf("newInstrumentState: %v", err)
	}

	newSavePath := filepath.Join(newDir, "preset.yaml")
	if _, err := state.saveConfig(newSavePath); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	wantRel, err := filepath.Rel(newDir, assetPath)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	got := state.cfg.Audio.Layers[0].Path
	if got != filepath.Clean(wantRel) {
		t.Fatalf("rebased path mismatch: got %q want %q", got, filepath.Clean(wantRel))
	}
	if state.layers["custom"].resolvedPath != assetPath {
		t.Fatalf("runtime resolved path changed unexpectedly: got %q want %q", state.layers["custom"].resolvedPath, assetPath)
	}
}

func TestManagedRecordingDeleteStillWorksAfterSavePathChange(t *testing.T) {
	rootDir := t.TempDir()
	oldDir := filepath.Join(rootDir, "old")
	newDir := filepath.Join(rootDir, "new")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatalf("mkdir old dir: %v", err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatalf("mkdir new dir: %v", err)
	}

	cfg := defaultInstrumentConfig("127.0.0.1:8765", 0.05)
	cfg.Audio.Layers = nil

	state, err := newInstrumentState(cfg, time.Second, filepath.Join(oldDir, "preset.yaml"))
	if err != nil {
		t.Fatalf("newInstrumentState: %v", err)
	}

	resp, err := state.createRecording("take one", []byte("fake wav bytes"))
	if err != nil {
		t.Fatalf("createRecording: %v", err)
	}

	layer := state.layers[resp.LayerID]
	if layer == nil {
		t.Fatal("expected recording layer to exist")
	}
	recordingPath := layer.resolvedPath
	if _, err := os.Stat(recordingPath); err != nil {
		t.Fatalf("expected recording file to exist: %v", err)
	}

	if _, err := state.saveConfig(filepath.Join(newDir, "preset.yaml")); err != nil {
		t.Fatalf("saveConfig after recording: %v", err)
	}
	if _, err := state.deleteManagedLayer(resp.LayerID); err != nil {
		t.Fatalf("deleteManagedLayer: %v", err)
	}
	if _, err := os.Stat(recordingPath); !os.IsNotExist(err) {
		t.Fatalf("expected recording file to be deleted, got err=%v", err)
	}
	if _, ok := state.layers[resp.LayerID]; ok {
		t.Fatal("expected recording layer to be removed from runtime state")
	}
}

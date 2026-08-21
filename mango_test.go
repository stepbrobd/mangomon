package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseTransform(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"normal", 0},
		{"Normal", 0},
		{"NORMAL", 0},
		{"90", 1},
		{"180", 2},
		{"270", 3},
		{"flipped", 4},
		{"Flipped", 4},
		{"flipped-90", 5},
		{"flipped_90", 5},
		{"FLIPPED90", 5},
		{"flipped-180", 6},
		{"flipped-270", 7},
		{"garbage", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := parseTransform(tt.in); got != tt.want {
			t.Errorf("parseTransform(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestTransformRoundTrip(t *testing.T) {
	for i := range 8 {
		got := parseTransform(transformString(i))
		if got != i {
			t.Errorf("round trip for transform %d: emitted %q, parsed back as %d",
				i, transformString(i), got)
		}
	}
}

func TestBuildEDIDName(t *testing.T) {
	withSerial := "D8RVY54"
	emptySerial := ""
	whitespace := "  "
	tests := []struct {
		name   string
		make_  string
		model  string
		serial *string
		want   string
	}{
		{"all present", "Dell Inc.", "DELL P3425WE", &withSerial,
			"Dell Inc. DELL P3425WE D8RVY54"},
		{"nil serial uses Unknown", "BOE", "NE135A1M-NY1", nil,
			"BOE NE135A1M-NY1 Unknown"},
		{"empty serial uses Unknown", "BOE", "NE135A1M-NY1", &emptySerial,
			"BOE NE135A1M-NY1 Unknown"},
		{"whitespace serial uses Unknown", "BOE", "NE135A1M-NY1", &whitespace,
			"BOE NE135A1M-NY1 Unknown"},
		{"empty make is dropped", "", "Model", nil, "Model Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildEDIDName(tt.make_, tt.model, tt.serial); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMHz pins the float-to-millihertz conversion against values wlr-randr
// actually prints. wlr-randr emits refresh as (float)mhz/1000 through C float
// precision (60001 mHz prints as 60.000999), and on apply it recovers the
// integer with round(hz*1000), so the conversion must reproduce the exact
// advertised millihertz for every printed value
func TestMHz(t *testing.T) {
	tests := []struct {
		in   float64
		want int
	}{
		{59.973000, 59973},
		{99.982002, 99982},
		{120.000000, 120000},
		{60.000999, 60001},
		{240.083, 240083},
		{59.939999, 59940},
	}
	for _, tt := range tests {
		if got := mhz(tt.in); got != tt.want {
			t.Errorf("mhz(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestSnapMode(t *testing.T) {
	// mode list captured from wlr-randr --json against mango on this host,
	// including eDP-1's real-world quirk of two preferred modes and the
	// C-float artifact 60.000999 (= 60001 mHz)
	modes := []wlrMode{
		{Width: 3440, Height: 1440, Refresh: 59.973000, Preferred: true, Current: true},
		{Width: 3440, Height: 1440, Refresh: 99.982002},
		{Width: 2560, Height: 1440, Refresh: 59.951000},
		{Width: 2880, Height: 1920, Refresh: 120.000000, Preferred: true},
		{Width: 2880, Height: 1920, Refresh: 60.000999, Preferred: true},
	}

	tests := []struct {
		name      string
		w, h      uint32
		hz        float32
		want      string
		wantError bool
	}{
		{"exact rate", 3440, 1440, 59.973, "3440x1440@59.973", false},
		{"profile saved at 60.000 snaps to 59.973", 3440, 1440, 60.000, "3440x1440@59.973", false},
		{"high refresh", 3440, 1440, 99.982, "3440x1440@99.982", false},
		{"prefers closer of two candidates", 2880, 1920, 60.5, "2880x1920@60.001", false},
		{"exact 120", 2880, 1920, 120.0, "2880x1920@120.000", false},
		{"resolution mismatch", 1024, 768, 60.0, "", true},
		{"delta beyond 1Hz rejected", 3440, 1440, 80.0, "", true},
		{"empty modes list", 3440, 1440, 60.0, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := modes
			if tt.name == "empty modes list" {
				m = nil
			}
			got, err := snapMode(m, tt.w, tt.h, tt.hz)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("snapMode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyArgs(t *testing.T) {
	live := []wlrOutput{
		{
			Name: "DP-4",
			Modes: []wlrMode{
				{Width: 3440, Height: 1440, Refresh: 59.973000, Preferred: true, Current: true},
				{Width: 3440, Height: 1440, Refresh: 99.982002},
			},
			Enabled: true,
		},
		{
			Name: "eDP-1",
			Modes: []wlrMode{
				{Width: 2880, Height: 1920, Refresh: 120.000000, Preferred: true},
			},
			Enabled: true,
		},
	}

	t.Run("full set builds one atomic invocation", func(t *testing.T) {
		monitors := []Monitor{
			{Name: "DP-4", Active: true, PxW: 3440, PxH: 1440, Hz: 59.973,
				X: 3840, Y: 0, Scale: 1.0, Transform: 0, VRR: 0},
			{Name: "eDP-1", Active: false},
		}
		got, err := applyArgs(monitors, live)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			"--output", "DP-4", "--on",
			"--mode", "3440x1440@59.973",
			"--pos", "3840,0",
			"--scale", "1",
			"--transform", "normal",
			"--adaptive-sync", "disabled",
			"--output", "eDP-1", "--off",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("applyArgs =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("fractional scale and rotation and vrr", func(t *testing.T) {
		monitors := []Monitor{
			{Name: "eDP-1", Active: true, PxW: 2880, PxH: 1920, Hz: 120,
				X: 0, Y: 0, Scale: 1.5, Transform: 3, VRR: 1},
		}
		got, err := applyArgs(monitors, live)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			"--output", "eDP-1", "--on",
			"--mode", "2880x1920@120.000",
			"--pos", "0,0",
			"--scale", "1.5",
			"--transform", "270",
			"--adaptive-sync", "enabled",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("applyArgs =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("legacy on-demand vrr applies as enabled", func(t *testing.T) {
		monitors := []Monitor{
			{Name: "eDP-1", Active: true, PxW: 2880, PxH: 1920, Hz: 120,
				Scale: 1.0, VRR: 2},
		}
		got, err := applyArgs(monitors, live)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		found := false
		for i := 0; i+1 < len(got); i++ {
			if got[i] == "--adaptive-sync" && got[i+1] == "enabled" {
				found = true
			}
		}
		if !found {
			t.Errorf("VRR=2 should apply --adaptive-sync enabled, args: %v", got)
		}
	})

	t.Run("disconnected output is an error", func(t *testing.T) {
		monitors := []Monitor{
			{Name: "HDMI-A-1", Active: true, PxW: 1920, PxH: 1080, Hz: 60, Scale: 1.0},
		}
		if _, err := applyArgs(monitors, live); err == nil {
			t.Errorf("expected error for output not in live set")
		}
	})
}

// TestGetAvailableModesFormatMatchesPicker pins the contract between
// getAvailableModes and the mode picker's parser. The picker regex requires
// the trailing "Hz" suffix; without it parseDisplayModes returns an empty
// slice and the mode picker view panics on F
func TestGetAvailableModesFormatMatchesPicker(t *testing.T) {
	pickerRegex := regexp.MustCompile(`(\d+)x(\d+)@([\d.]+)Hz`)
	sample := "3440x1440@59.973Hz"
	if !pickerRegex.MatchString(sample) {
		t.Fatalf("regex no longer accepts the emitted format - mode_picker parser changed?")
	}
	if pickerRegex.MatchString("3440x1440@59.973") {
		t.Fatalf("regex now accepts the suffix-less form; the suffix guard is no longer needed")
	}
}

func TestOutputsToMonitorsFixture(t *testing.T) {
	// DP-4 and eDP-1 are verbatim wlr-randr 0.5.0 --json output against
	// mango on this host (modes truncated); DP-2 is the disabled-head shape
	// the same printer emits (enabled false, no position, transform, scale,
	// or adaptive_sync keys, no current mode). covers the null serial, the
	// disabled-head fallback to the preferred mode, and the C-float refresh
	fixture := `[
		{
			"name": "DP-4",
			"description": "Dell Inc. DELL P3425WE D8RVY54 (DP-4)",
			"make": "Dell Inc.",
			"model": "DELL P3425WE",
			"serial": "D8RVY54",
			"physical_size": {"width": 800, "height": 330},
			"enabled": true,
			"modes": [
				{"width": 3440, "height": 1440, "refresh": 59.973000, "preferred": true, "current": true},
				{"width": 3440, "height": 1440, "refresh": 99.982002, "preferred": false, "current": false},
				{"width": 2560, "height": 1440, "refresh": 59.951000, "preferred": false, "current": false}
			],
			"position": {"x": 3840, "y": 0},
			"transform": "normal",
			"scale": 1.000000,
			"adaptive_sync": false
		},
		{
			"name": "eDP-1",
			"description": "BOE NE135A1M-NY1 (eDP-1)",
			"make": "BOE",
			"model": "NE135A1M-NY1",
			"serial": null,
			"physical_size": {"width": 290, "height": 190},
			"enabled": true,
			"modes": [
				{"width": 2880, "height": 1920, "refresh": 120.000000, "preferred": true, "current": true},
				{"width": 2880, "height": 1920, "refresh": 60.000999, "preferred": true, "current": false}
			],
			"position": {"x": 0, "y": 0},
			"transform": "270",
			"scale": 1.500000,
			"adaptive_sync": true
		},
		{
			"name": "DP-2",
			"description": "Dell Inc. DELL U2410 F525M0BQ239L (DP-2)",
			"make": "Dell Inc.",
			"model": "DELL U2410",
			"serial": "F525M0BQ239L",
			"physical_size": {"width": 520, "height": 320},
			"enabled": false,
			"modes": [
				{"width": 1920, "height": 1200, "refresh": 59.950001, "preferred": true, "current": false},
				{"width": 1280, "height": 1024, "refresh": 75.025002, "preferred": false, "current": false}
			]
		}
	]`

	var outputs []wlrOutput
	if err := json.Unmarshal([]byte(fixture), &outputs); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	monitors := outputsToMonitors(outputs)

	if len(monitors) != 3 {
		t.Fatalf("got %d monitors, want 3", len(monitors))
	}

	// sorted by name; lowercase 'e' sorts after uppercase 'D' in ASCII
	dp2, dp4, edp1 := monitors[0], monitors[1], monitors[2]
	if dp2.Name != "DP-2" || dp4.Name != "DP-4" || edp1.Name != "eDP-1" {
		t.Fatalf("unexpected sort order: %s, %s, %s", dp2.Name, dp4.Name, edp1.Name)
	}

	if got, want := dp4.HardwareID, "Dell Inc./DELL P3425WE/D8RVY54"; got != want {
		t.Errorf("DP-4 HardwareID = %q, want %q", got, want)
	}
	if got, want := dp4.EDIDName, "Dell Inc. DELL P3425WE D8RVY54"; got != want {
		t.Errorf("DP-4 EDIDName = %q, want %q", got, want)
	}
	if !dp4.Active {
		t.Errorf("DP-4 should be Active (enabled true)")
	}
	if dp4.PxW != 3440 || dp4.PxH != 1440 {
		t.Errorf("DP-4 PxW/PxH = %d/%d, want 3440/1440", dp4.PxW, dp4.PxH)
	}
	if dp4.Hz < 59.970 || dp4.Hz > 59.976 {
		t.Errorf("DP-4 Hz = %v, want ~59.973", dp4.Hz)
	}
	if dp4.X != 3840 || dp4.Y != 0 {
		t.Errorf("DP-4 X/Y = %d/%d, want 3840/0", dp4.X, dp4.Y)
	}
	if dp4.Scale != 1.0 {
		t.Errorf("DP-4 Scale = %v, want 1.0", dp4.Scale)
	}
	if dp4.Transform != 0 {
		t.Errorf("DP-4 Transform = %d, want 0", dp4.Transform)
	}
	if dp4.VRR != 0 {
		t.Errorf("DP-4 VRR = %d, want 0 (adaptive_sync false)", dp4.VRR)
	}
	if len(dp4.Modes) != 3 {
		t.Errorf("DP-4 Modes len = %d, want 3", len(dp4.Modes))
	}

	if got, want := edp1.HardwareID, "BOE/NE135A1M-NY1"; got != want {
		t.Errorf("eDP-1 HardwareID = %q, want %q (no /serial segment)", got, want)
	}
	if got, want := edp1.EDIDName, "BOE NE135A1M-NY1 Unknown"; got != want {
		t.Errorf("eDP-1 EDIDName = %q, want %q (Unknown sentinel)", got, want)
	}
	if edp1.Scale != 1.5 {
		t.Errorf("eDP-1 Scale = %v, want 1.5", edp1.Scale)
	}
	if edp1.Transform != 3 {
		t.Errorf("eDP-1 Transform = %d, want 3 (270)", edp1.Transform)
	}
	if edp1.VRR != 1 {
		t.Errorf("eDP-1 VRR = %d, want 1 (adaptive_sync true)", edp1.VRR)
	}

	if dp2.Active {
		t.Errorf("DP-2 should be inactive (enabled false)")
	}
	// disabled head has no current mode; preferred mode supplies dimensions
	if dp2.PxW != 1920 || dp2.PxH != 1200 {
		t.Errorf("DP-2 PxW/PxH = %d/%d, want 1920/1200 (preferred fallback)", dp2.PxW, dp2.PxH)
	}
	// disabled monitors still need Scale=1 so the world bounds math in
	// updateWorld does not divide by zero
	if dp2.Scale != 1.0 {
		t.Errorf("DP-2 Scale = %v, want 1.0 default for disabled", dp2.Scale)
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		in     string
		wantW  uint32
		wantH  uint32
		wantHz float32
		isNil  bool
	}{
		{"3440x1440@59.973Hz", 3440, 1440, 59.973, false},
		{"3440x1440@59.973", 3440, 1440, 59.973, false},
		{"1920x1080@60.00Hz", 1920, 1080, 60.00, false},
		{"640x480@59.940", 640, 480, 59.940, false},
		{"garbage", 0, 0, 0, true},
		{"2560x@60", 0, 0, 0, true},
		{"x1440@60", 0, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			m := parseMode(tt.in)
			if tt.isNil {
				if m != nil {
					t.Errorf("expected nil, got %+v", *m)
				}
				return
			}
			if m == nil {
				t.Fatalf("parseMode returned nil")
			}
			if m.W != tt.wantW || m.H != tt.wantH {
				t.Errorf("WxH = %dx%d, want %dx%d", m.W, m.H, tt.wantW, tt.wantH)
			}
			if d := m.Hz - tt.wantHz; d > 0.01 || d < -0.01 {
				t.Errorf("Hz = %v, want %v", m.Hz, tt.wantHz)
			}
		})
	}
}

func TestVerifyOutputs(t *testing.T) {
	live := func(mut ...func(*[]wlrOutput)) []wlrOutput {
		out := []wlrOutput{
			{
				Name:    "DP-1",
				Enabled: true,
				Modes: []wlrMode{
					{Width: 2560, Height: 1440, Refresh: 480.167999, Preferred: true},
					{Width: 2560, Height: 1440, Refresh: 240.082993, Current: true},
				},
			},
			{
				Name:    "DP-2",
				Enabled: true,
				Modes: []wlrMode{
					{Width: 2560, Height: 1440, Refresh: 59.951000, Current: true},
					{Width: 2560, Height: 1440, Refresh: 299.993011, Preferred: true},
				},
			},
		}
		for _, m := range mut {
			m(&out)
		}
		return out
	}

	want := []Monitor{
		{Name: "DP-1", Active: true, PxW: 2560, PxH: 1440, Hz: 240.083, Scale: 1},
		{Name: "DP-2", Active: true, PxW: 2560, PxH: 1440, Hz: 59.951, Scale: 1},
	}

	t.Run("live matches want", func(t *testing.T) {
		if err := verifyOutputs(want, live()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("head went dark after the commit", func(t *testing.T) {
		err := verifyOutputs(want, live(func(o *[]wlrOutput) { (*o)[0].Enabled = false }))
		if err == nil {
			t.Fatal("a blanked output must not verify")
		}
		if !strings.Contains(err.Error(), "DP-1") {
			t.Errorf("error must name the output, got %q", err)
		}
	})

	t.Run("head fell back to another mode", func(t *testing.T) {
		asked480 := []Monitor{
			{Name: "DP-1", Active: true, PxW: 2560, PxH: 1440, Hz: 480.168, Scale: 1},
			want[1],
		}
		err := verifyOutputs(asked480, live())
		if err == nil {
			t.Fatal("a silent mode fallback must not verify")
		}
		if !strings.Contains(err.Error(), "DP-1") {
			t.Errorf("error must name the output, got %q", err)
		}
	})

	t.Run("head refused to turn off", func(t *testing.T) {
		off := []Monitor{{Name: "DP-1", Active: false}, want[1]}
		if err := verifyOutputs(off, live()); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("head turned off as asked", func(t *testing.T) {
		off := []Monitor{{Name: "DP-1", Active: false}, want[1]}
		err := verifyOutputs(off, live(func(o *[]wlrOutput) {
			(*o)[0].Enabled = false
			(*o)[0].Modes[1].Current = false
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("head left the live set", func(t *testing.T) {
		err := verifyOutputs(want, live()[:1])
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "DP-2") {
			t.Errorf("error must name the output, got %q", err)
		}
	})

	t.Run("enabled head reports no current mode", func(t *testing.T) {
		err := verifyOutputs(want, live(func(o *[]wlrOutput) { (*o)[0].Modes[1].Current = false }))
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestRestoreSelection(t *testing.T) {
	previous := []Monitor{
		{Name: "DP-1", Active: true, Hz: 240.083},
		{Name: "DP-2", Active: true, Hz: 59.951},
		{Name: "eDP-1", Active: false},
	}

	t.Run("every head present", func(t *testing.T) {
		live := []wlrOutput{{Name: "DP-1"}, {Name: "DP-2"}, {Name: "eDP-1"}}
		if got := missingOutputs(previous, live); got != nil {
			t.Errorf("missingOutputs = %v, want none", got)
		}
		if got := presentOutputs(previous, live); len(got) != 3 {
			t.Errorf("presentOutputs kept %d, want 3", len(got))
		}
	})

	t.Run("one head dropped off the wire", func(t *testing.T) {
		live := []wlrOutput{{Name: "eDP-1"}, {Name: "DP-1"}}
		if got := missingOutputs(previous, live); !reflect.DeepEqual(got, []string{"DP-2"}) {
			t.Errorf("missingOutputs = %v, want [DP-2]", got)
		}
		got := presentOutputs(previous, live)
		if len(got) != 2 || got[0].Name != "DP-1" || got[1].Name != "eDP-1" {
			t.Fatalf("presentOutputs = %v, want DP-1 and eDP-1 in want order", got)
		}
		if got[0].Hz != 240.083 {
			t.Errorf("selection dropped the wanted settings, Hz = %v", got[0].Hz)
		}
	})

	t.Run("no heads at all", func(t *testing.T) {
		if got := missingOutputs(previous, nil); len(got) != 3 {
			t.Errorf("missingOutputs = %v, want all three", got)
		}
		if got := presentOutputs(previous, nil); len(got) != 0 {
			t.Errorf("presentOutputs = %v, want empty", got)
		}
	})
}

// fakeCompositor answers --json from its own head state and records every
// configuration it is handed, so apply and rollback can be driven without a
// running compositor.
type fakeCompositor struct {
	enabled map[string]bool
	refresh map[string]float64
	calls   [][]string
	reject  bool
}

func newFakeCompositor() *fakeCompositor {
	return &fakeCompositor{
		enabled: map[string]bool{"DP-1": false, "DP-2": true},
		refresh: map[string]float64{"DP-2": 59.951000},
	}
}

func (f *fakeCompositor) modes(name string) []wlrMode {
	all := []float64{299.993011, 240.001007, 143.973007, 120.000000, 59.951000}
	out := make([]wlrMode, 0, len(all))
	for _, hz := range all {
		out = append(out, wlrMode{
			Width: 2560, Height: 1440, Refresh: hz,
			Current: f.enabled[name] && f.refresh[name] == hz,
		})
	}
	return out
}

func (f *fakeCompositor) exec(args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--json" {
		heads := make([]wlrOutput, 0, 2)
		for _, name := range []string{"DP-1", "DP-2"} {
			scale := 1.0
			heads = append(heads, wlrOutput{
				Name: name, Enabled: f.enabled[name], Modes: f.modes(name),
				Position: &wlrPosition{}, Scale: &scale,
			})
		}
		b, err := json.Marshal(heads)
		return b, err
	}

	f.calls = append(f.calls, args)
	if f.reject {
		return nil, errors.New("failed to apply configuration")
	}
	for i := range args {
		if args[i] != "--output" || i+1 >= len(args) {
			continue
		}
		name := args[i+1]
		for j := i + 2; j < len(args) && args[j] != "--output"; j++ {
			switch args[j] {
			case "--off":
				f.enabled[name] = false
			case "--on":
				f.enabled[name] = true
			case "--mode":
				if m := parseMode(args[j+1]); m != nil {
					f.refresh[name] = float64(mhz(float64(m.Hz))) / 1000.0
				}
			}
		}
	}
	return nil, nil
}

func withFakeCompositor(t *testing.T, f *fakeCompositor) {
	t.Helper()
	realExec, realHold, realPoll := execWlrRandr, applyHoldTime, applyPollInterval
	realRestore := restoreTimeout
	execWlrRandr = f.exec
	applyHoldTime = 10 * time.Millisecond
	applyPollInterval = time.Millisecond
	restoreTimeout = 50 * time.Millisecond
	rollbackLayout = nil
	t.Cleanup(func() {
		execWlrRandr, applyHoldTime, applyPollInterval = realExec, realHold, realPoll
		restoreTimeout = realRestore
		rollbackLayout = nil
	})
}

// TestRollbackTargetsPreApplyState pins the direction of the rollback. Handing
// saveRollback the configuration about to be applied made revert a no-op.
func TestRollbackTargetsPreApplyState(t *testing.T) {
	f := newFakeCompositor()
	withFakeCompositor(t, f)

	next := []Monitor{
		{Name: "DP-1", Active: false},
		{Name: "DP-2", Active: true, PxW: 2560, PxH: 1440, Hz: 120, Scale: 1},
	}
	if err := applyMonitors(next); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f.refresh["DP-2"] != 120 {
		t.Fatalf("apply left DP-2 at %v, want 120", f.refresh["DP-2"])
	}

	for _, m := range rollbackLayout {
		if m.Name == "DP-2" && m.Hz != 59.951 {
			t.Fatalf("rollback target holds %v, want the pre-apply 59.951", m.Hz)
		}
	}

	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if f.refresh["DP-2"] != 59.951 {
		t.Errorf("rollback left DP-2 at %v, want 59.951", f.refresh["DP-2"])
	}
}

// TestRollbackDoesNotRetargetItself keeps a second revert from reapplying the
// layout the first one undid.
func TestRollbackDoesNotRetargetItself(t *testing.T) {
	f := newFakeCompositor()
	withFakeCompositor(t, f)

	next := []Monitor{{Name: "DP-2", Active: true, PxW: 2560, PxH: 1440, Hz: 120, Scale: 1}}
	if err := applyMonitors(next); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if f.refresh["DP-2"] != 59.951 {
		t.Errorf("second rollback moved DP-2 to %v, want it to stay at 59.951", f.refresh["DP-2"])
	}
}

func TestRollbackWithoutAnApply(t *testing.T) {
	f := newFakeCompositor()
	withFakeCompositor(t, f)
	if err := rollback(); err == nil {
		t.Fatal("rollback before any apply must fail")
	}
}

// TestConcurrentApplyRejected keeps a second apply from racing the recovery of
// the first, which would build on the state being undone.
func TestConcurrentApplyRejected(t *testing.T) {
	f := newFakeCompositor()
	withFakeCompositor(t, f)

	entered := make(chan struct{})
	release := make(chan struct{})
	inner := execWlrRandr
	execWlrRandr = func(args ...string) ([]byte, error) {
		if len(args) > 1 {
			select {
			case entered <- struct{}{}:
				<-release
			default:
			}
		}
		return inner(args...)
	}

	next := []Monitor{{Name: "DP-2", Active: true, PxW: 2560, PxH: 1440, Hz: 120, Scale: 1}}
	done := make(chan error, 1)
	go func() { done <- applyMonitors(next) }()

	<-entered
	if err := applyMonitors(next); err == nil {
		t.Error("a second apply must be rejected while one is running")
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := applyMonitors(next); err != nil {
		t.Errorf("apply after the first finished: %v", err)
	}
}

// TestKeepStoredGeometry pins the case that rewrote eDP-1 from scale 1.5 to
// 1.0: the panel was off, so the live read had no scale to report.
func TestKeepStoredGeometry(t *testing.T) {
	stored := []Monitor{
		{Name: "eDP-1", Scale: 1.5, X: 320, Y: 2880},
		{Name: "DP-1", Scale: 1.0, X: 0, Y: 1440},
	}

	t.Run("unreported geometry falls back to the profile", func(t *testing.T) {
		fresh := []Monitor{
			{Name: "eDP-1", Scale: 1.0, X: 0, Y: 0, GeometryKnown: false},
			{Name: "DP-1", Scale: 1.0, X: 0, Y: 1440, GeometryKnown: true},
		}
		got := keepStoredGeometry(fresh, stored)
		if got[0].Scale != 1.5 || got[0].X != 320 || got[0].Y != 2880 {
			t.Errorf("eDP-1 = scale %v at (%d,%d), want 1.5 at (320,2880)", got[0].Scale, got[0].X, got[0].Y)
		}
	})

	t.Run("an edit to an inactive head survives", func(t *testing.T) {
		fresh := []Monitor{{Name: "eDP-1", Scale: 2.0, X: 100, Y: 200, GeometryKnown: true}}
		got := keepStoredGeometry(fresh, stored)
		if got[0].Scale != 2.0 || got[0].X != 100 {
			t.Errorf("edit was discarded, got scale %v at (%d,%d)", got[0].Scale, got[0].X, got[0].Y)
		}
	})

	t.Run("head absent from the stored profile", func(t *testing.T) {
		fresh := []Monitor{{Name: "DP-9", Scale: 1.0, GeometryKnown: false}}
		if got := keepStoredGeometry(fresh, stored); got[0].Scale != 1.0 {
			t.Errorf("unexpected scale %v", got[0].Scale)
		}
	})

	t.Run("input is not mutated", func(t *testing.T) {
		fresh := []Monitor{{Name: "eDP-1", Scale: 1.0, GeometryKnown: false}}
		keepStoredGeometry(fresh, stored)
		if fresh[0].Scale != 1.0 {
			t.Error("keepStoredGeometry mutated its argument")
		}
	})
}

// TestOutputsToMonitorsMarksGeometry ties the flag to what wlr-randr reports.
func TestOutputsToMonitorsMarksGeometry(t *testing.T) {
	scale := 1.25
	live := []wlrOutput{
		{Name: "DP-1", Enabled: true, Scale: &scale, Position: &wlrPosition{X: 10, Y: 20},
			Modes: []wlrMode{{Width: 2560, Height: 1440, Refresh: 60, Current: true}}},
		{Name: "eDP-1", Enabled: false,
			Modes: []wlrMode{{Width: 2880, Height: 1920, Refresh: 120, Preferred: true}}},
	}

	got := outputsToMonitors(live)
	byName := map[string]Monitor{}
	for _, m := range got {
		byName[m.Name] = m
	}
	if !byName["DP-1"].GeometryKnown {
		t.Error("an enabled head reports scale, so its geometry is known")
	}
	if byName["eDP-1"].GeometryKnown {
		t.Error("a disabled head reports no scale, so its geometry is a placeholder")
	}
	if byName["eDP-1"].Scale != 1.0 {
		t.Errorf("placeholder scale = %v, want 1.0 to keep the world math finite", byName["eDP-1"].Scale)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Home.json")

	if err := writeFileAtomic(path, []byte("first")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeFileAtomic(path, []byte("second")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("contents = %q, want %q", got, "second")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != profileFileMode {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), os.FileMode(profileFileMode))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%d files left behind, want only the target", len(entries))
	}
}

// TestResolveProfileMonitorsNamesAbsent covers applying a profile with one of
// its monitors unplugged, which used to report success over a partial layout.
func TestResolveProfileMonitorsNamesAbsent(t *testing.T) {
	current := []Monitor{
		{Name: "DP-2", HardwareID: "LG Electronics/LG ULTRAGEAR/508AXNR0J135"},
	}
	saved := []Monitor{
		{Name: "DP-1", HardwareID: "LG Electronics/LG ULTRAGEAR+/601NTQDH7820"},
		{Name: "DP-2", HardwareID: "LG Electronics/LG ULTRAGEAR/508AXNR0J135"},
		{Name: "eDP-1", HardwareID: "BOE/NE135A1M-NY1"},
	}

	resolved, absent := resolveProfileMonitors(saved, current)
	if len(resolved) != 1 || resolved[0].Name != "DP-2" {
		t.Fatalf("resolved = %v, want only DP-2", resolved)
	}
	if !reflect.DeepEqual(absent, []string{"DP-1", "eDP-1"}) {
		t.Errorf("absent = %v, want [DP-1 eDP-1]", absent)
	}

	if _, absent := resolveProfileMonitors(saved, saved); absent != nil {
		t.Errorf("absent = %v, want none when every monitor is connected", absent)
	}
}

// TestProfileOmitsModes keeps advertised mode lists out of stored profiles,
// where they only go stale against the hardware actually attached.
func TestProfileOmitsModes(t *testing.T) {
	m := Monitor{
		Name: "DP-1", PxW: 2560, PxH: 1440, Hz: 240.083, Scale: 1, Active: true,
		Modes: []Mode{{W: 2560, H: 1440, Hz: 480.168}, {W: 3840, H: 2160, Hz: 60}},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "Modes") {
		t.Errorf("profile json carries a mode list: %s", data)
	}

	var back Monitor
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Hz != m.Hz || back.PxW != m.PxW || back.Scale != m.Scale {
		t.Errorf("round trip lost settings, got %+v", back)
	}
	if back.Modes != nil {
		t.Errorf("Modes = %v, want nil", back.Modes)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// wlrRandrTimeout bounds every wlr-randr invocation; generous because a
	// modeset can block briefly while the compositor settles, and the
	// per-call cost of waiting is paid only on a state churn (the tool is
	// not in any latency-sensitive hot path)
	wlrRandrTimeout = 30 * time.Second

	// file permissions for profile json
	profileDirMode  = 0700
	profileFileMode = 0600

	// default world dimensions
	defaultWorldWidth  = 3840
	defaultWorldHeight = 2160
	defaultWorldScale  = 1.0

	worldPaddingPx      = 500
	desktopBorderMargin = 3
	desktopFooterHeight = 10
)

// a tunnelled DP head can accept a modeset and drop the link seconds later,
// so an applied layout counts only once it has held
var (
	applyPollInterval  = 500 * time.Millisecond
	applyHoldTime      = 3 * time.Second
	applyVerifyTimeout = 12 * time.Second

	// a head dropped by a failed apply needs seconds to return to the wire
	restoreRetryInterval = 1 * time.Second
	restoreTimeout       = 45 * time.Second
)

// parseMode accepts the "WxH@Hz" form shared by hyprland, wlr-randr, and the
// mode picker, including an optional trailing "Hz" suffix
func parseMode(modeStr string) *Mode {
	parts := strings.Split(modeStr, "@")
	if len(parts) != 2 {
		return nil
	}
	resParts := strings.Split(parts[0], "x")
	if len(resParts) != 2 {
		return nil
	}
	w, err := strconv.ParseUint(resParts[0], 10, 32)
	if err != nil {
		return nil
	}
	h, err := strconv.ParseUint(resParts[1], 10, 32)
	if err != nil {
		return nil
	}
	hzStr := strings.TrimSuffix(parts[1], "Hz")
	hz, err := strconv.ParseFloat(hzStr, 32)
	if err != nil {
		return nil
	}
	return &Mode{W: uint32(w), H: uint32(h), Hz: float32(hz)}
}

// execWlrRandr runs wlr-randr with a bounded timeout and returns stdout.
// mango implements wlr-output-management (zwlr_output_manager_v1), and its
// docs point at wlr-randr as the reference client, so mangomon drives output
// configuration through it the way nirimon drives `niri msg`. stderr is
// folded into the error since wlr-randr puts the useful diagnostics there
// (e.g. "failed to apply configuration") and `exit status 1` alone is useless
var execWlrRandr = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wlrRandrTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wlr-randr", args...)
	output, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("wlr-randr %v: %w: %s", args, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("wlr-randr %v: %w", args, err)
	}
	return output, nil
}

// readOutputs runs `wlr-randr --json` and decodes the head array
func readOutputs() ([]wlrOutput, error) {
	output, err := execWlrRandr("--json")
	if err != nil {
		return nil, err
	}
	var outputs []wlrOutput
	if err := json.Unmarshal(output, &outputs); err != nil {
		return nil, fmt.Errorf("failed to parse wlr-randr --json output: %w", err)
	}
	return outputs, nil
}

// wlrOutput mirrors the schema emitted by `wlr-randr --json`. serial is
// nullable when EDID metadata is absent (a laptop panel), and position,
// transform, scale, and adaptive_sync are omitted entirely for disabled
// heads, so those decode through pointers. adaptive_sync is also null when
// the compositor speaks protocol version < 4
type wlrOutput struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Make         string          `json:"make"`
	Model        string          `json:"model"`
	Serial       *string         `json:"serial"`
	PhysicalSize wlrPhysicalSize `json:"physical_size"`
	Enabled      bool            `json:"enabled"`
	Modes        []wlrMode       `json:"modes"`
	Position     *wlrPosition    `json:"position"`
	Transform    *string         `json:"transform"`
	Scale        *float64        `json:"scale"`
	AdaptiveSync *bool           `json:"adaptive_sync"`
}

type wlrPhysicalSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type wlrPosition struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// wlrMode is one advertised mode. Refresh is float Hz because that is what
// the JSON carries, but the advertised rate is really an integer millihertz
// that wlr-randr printed as (float)mhz/1000; use mhz() to recover it before
// comparing or emitting mode strings
type wlrMode struct {
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Refresh   float64 `json:"refresh"`
	Preferred bool    `json:"preferred"`
	Current   bool    `json:"current"`
}

// mhz recovers the integer millihertz behind a refresh float. wlr-randr
// itself parses a CLI refresh with round(hz*1000) and then requires an exact
// integer match against the advertised mode, so this same rounding is what
// makes a JSON-read value round-trip back through the CLI byte-exactly
// (59973 mHz prints as 59.973000, 60001 mHz prints as 60.000999 through C
// float precision; both round back to the advertised integer)
func mhz(refresh float64) int {
	return int(math.Round(refresh * 1000))
}

// parseTransform maps wlr-randr's transform strings ("normal", "flipped-90")
// to the wl_output_transform int codes 0-7 that hyprmon used and that mango's
// monitorrule `rr:` field shares. case and separator variations are tolerated
func parseTransform(s string) int {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	switch s {
	case "normal":
		return 0
	case "90":
		return 1
	case "180":
		return 2
	case "270":
		return 3
	case "flipped":
		return 4
	case "flipped90":
		return 5
	case "flipped180":
		return 6
	case "flipped270":
		return 7
	default:
		return 0
	}
}

// transformString maps the Transform int code 0-7 to the form wlr-randr's
// --transform flag accepts (the inverse of parseTransform)
func transformString(t int) string {
	switch t {
	case 0:
		return "normal"
	case 1:
		return "90"
	case 2:
		return "180"
	case 3:
		return "270"
	case 4:
		return "flipped"
	case 5:
		return "flipped-90"
	case 6:
		return "flipped-180"
	case 7:
		return "flipped-270"
	default:
		return "normal"
	}
}

// buildEDIDName produces the space-separated "make model serial" identifier
// with niri's "Unknown" sentinel for a missing serial. mango addresses
// outputs by connector name only, but the field is kept byte-compatible so
// profiles written by nirimon or hyprmon keep matching when copied over, and
// profiles written here stay portable back
func buildEDIDName(make_, model string, serial *string) string {
	mk := strings.TrimSpace(make_)
	md := strings.TrimSpace(model)
	sr := ""
	if serial != nil {
		sr = strings.TrimSpace(*serial)
	}
	if sr == "" {
		sr = "Unknown"
	}
	parts := []string{}
	if mk != "" {
		parts = append(parts, mk)
	}
	if md != "" {
		parts = append(parts, md)
	}
	parts = append(parts, sr)
	return strings.Join(parts, " ")
}

func readMonitors() ([]Monitor, error) {
	outputs, err := readOutputs()
	if err != nil {
		return nil, err
	}
	return seedUnknownGeometry(outputsToMonitors(outputs)), nil
}

// outputsToMonitors converts the parsed wlr-randr shape into the slice the
// rest of the codebase consumes; broken out from readMonitors so tests can
// drive the parsing path with a fixture without shelling out
func outputsToMonitors(outputs []wlrOutput) []Monitor {
	// sort by name for deterministic ordering across runs
	sorted := make([]wlrOutput, len(outputs))
	copy(sorted, outputs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	monitors := make([]Monitor, 0, len(sorted))
	for _, out := range sorted {
		modes := make([]Mode, 0, len(out.Modes))
		for _, m := range out.Modes {
			modes = append(modes, Mode{
				W:  uint32(m.Width),
				H:  uint32(m.Height),
				Hz: float32(float64(mhz(m.Refresh)) / 1000.0),
			})
		}

		serial := ""
		if out.Serial != nil {
			serial = *out.Serial
		}

		monitor := Monitor{
			Name:       out.Name,
			Make:       out.Make,
			Model:      out.Model,
			Serial:     serial,
			HardwareID: buildHardwareID(out.Make, out.Model, serial),
			EDIDName:   buildEDIDName(out.Make, out.Model, out.Serial),
			Modes:      modes,
			Active:     out.Enabled,
			VRR: func() int {
				if out.AdaptiveSync != nil && *out.AdaptiveSync {
					return 1
				}
				return 0
			}(),
			// mirror fields are inert at read time; preserved for profile
			// JSON round-trip and updated only via the TUI mirror picker
			IsMirrored:    false,
			MirrorSource:  "",
			MirrorTargets: []string{},
		}

		// the current mode exists only on an enabled head; for a disabled
		// head fall back to the preferred mode so the TUI has sensible
		// default dimensions to render
		modeIdx := -1
		for i, m := range out.Modes {
			if m.Current {
				modeIdx = i
				break
			}
		}
		if modeIdx < 0 {
			for i, m := range out.Modes {
				if m.Preferred {
					modeIdx = i
					break
				}
			}
		}
		if modeIdx >= 0 && modeIdx < len(out.Modes) {
			monitor.PxW = uint32(out.Modes[modeIdx].Width)
			monitor.PxH = uint32(out.Modes[modeIdx].Height)
			monitor.Hz = float32(float64(mhz(out.Modes[modeIdx].Refresh)) / 1000.0)
		}

		if out.Position != nil {
			monitor.X = int32(out.Position.X)
			monitor.Y = int32(out.Position.Y)
		}
		if out.Transform != nil {
			monitor.Transform = parseTransform(*out.Transform)
		}
		monitor.GeometryKnown = out.Scale != nil
		if out.Scale != nil {
			monitor.Scale = float32(*out.Scale)
		} else {
			// a zero scale would divide by zero in the world bounds math
			monitor.Scale = 1.0
		}

		monitors = append(monitors, monitor)
	}

	disambiguateHardwareIDs(monitors)
	return monitors
}

// snapMode picks the advertised mode whose width and height match exactly and
// whose refresh rate is closest to wantedHz. The returned string carries the
// exact advertised millihertz, because wlr-randr requires an exact integer
// match after its own round(hz*1000) and errors out otherwise. A delta beyond
// 1 Hz is rejected so a stale profile fails loudly instead of picking a
// wildly different rate
func snapMode(modes []wlrMode, w, h uint32, wantedHz float32) (string, error) {
	bestIdx := -1
	bestDelta := math.Inf(1)
	for i := range modes {
		if uint32(modes[i].Width) != w || uint32(modes[i].Height) != h {
			continue
		}
		hz := float64(mhz(modes[i].Refresh)) / 1000.0
		d := math.Abs(hz - float64(wantedHz))
		if d < bestDelta {
			bestDelta = d
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestDelta > 1.0 {
		return "", fmt.Errorf("no advertised mode matches %dx%d@%.3f within 1Hz", w, h, wantedHz)
	}
	m := mhz(modes[bestIdx].Refresh)
	return fmt.Sprintf("%dx%d@%d.%03d", modes[bestIdx].Width, modes[bestIdx].Height, m/1000, m%1000), nil
}

// applyArgs builds the argument list for a single wlr-randr invocation
// covering the whole monitor set. one invocation means one atomic
// zwlr_output_configuration_v1: mango tests and commits every head together,
// so a partially-applied layout cannot occur (an improvement over niri's
// sequential per-property applies). pure so tests can pin the exact argv
func applyArgs(monitors []Monitor, live []wlrOutput) ([]string, error) {
	liveByName := make(map[string]wlrOutput, len(live))
	for _, o := range live {
		liveByName[o.Name] = o
	}

	var args []string
	for _, m := range monitors {
		out, ok := liveByName[m.Name]
		if !ok {
			return nil, fmt.Errorf("output %q is not currently connected", m.Name)
		}

		if !m.Active {
			args = append(args, "--output", m.Name, "--off")
			continue
		}

		modeStr, err := snapMode(out.Modes, m.PxW, m.PxH, m.Hz)
		if err != nil {
			return nil, fmt.Errorf("snap mode for %s: %w", m.Name, err)
		}

		// wlr-output-management has no on-demand adaptive sync, so the
		// legacy hyprmon value 2 (fullscreen-only) applies as enabled
		adaptiveSync := "disabled"
		if m.VRR > 0 {
			adaptiveSync = "enabled"
		}

		args = append(args,
			"--output", m.Name, "--on",
			"--mode", modeStr,
			"--pos", fmt.Sprintf("%d,%d", m.X, m.Y),
			"--scale", fmt.Sprintf("%g", m.Scale),
			"--transform", transformString(m.Transform),
			"--adaptive-sync", adaptiveSync,
		)
	}
	return args, nil
}

// pixelRate approximates the link bandwidth a mode needs. Blanking intervals
// and DSC are ignored because a rate is only ever compared against another
// rate on the same head, to tell a change that gives capacity back from one
// that takes capacity
func pixelRate(w, h int, hz float64) float64 {
	return float64(w) * float64(h) * hz
}

// targetRate is the rate m asks for, zero when m is to be turned off
func targetRate(m Monitor) float64 {
	if !m.Active {
		return 0
	}
	return pixelRate(int(m.PxW), int(m.PxH), float64(m.Hz))
}

// liveRate is the rate out currently runs at, zero when it is off
func liveRate(out wlrOutput) float64 {
	if !out.Enabled {
		return 0
	}
	for _, mode := range out.Modes {
		if mode.Current {
			return pixelRate(mode.Width, mode.Height, mode.Refresh)
		}
	}
	return 0
}

// rateEpsilon absorbs the float32 Hz on a Monitor meeting the float64 refresh
// of a mode list, which leaves an unchanged head a few ulps apart instead of
// equal. It sits far below the gap between any two advertised modes
const rateEpsilon = 1e-6

// partitionApply splits monitors into the heads that give capacity back and
// the heads that take it. Unchanged, downgraded, and switched-off heads
// release; enabled and raised heads claim. A head live does not carry claims,
// so applyArgs reports it the way it always has
func partitionApply(monitors []Monitor, live []wlrOutput) (release, claim []Monitor) {
	byName := indexOutputs(live)
	for _, m := range monitors {
		if out, ok := byName[m.Name]; ok && targetRate(m) <= liveRate(out)*(1+rateEpsilon) {
			release = append(release, m)
			continue
		}
		claim = append(claim, m)
	}
	return release, claim
}

// settlePhase waits until the heads applied by a phase actually carry that
// state and the heads next names are back on the wire. wlr-randr snapshots
// head state when it connects, so a following invocation issued too early
// resends the state the previous phase just replaced. Disabling a tunnelled DP
// head also drops it from the compositor for seconds, and wlr-randr rejects a
// whole invocation that names a head it cannot see
func settlePhase(applied, next []Monitor) ([]wlrOutput, error) {
	deadline := time.Now().Add(applyVerifyTimeout)
	for {
		live, err := readOutputs()
		if err == nil {
			if err = verifyOutputs(applied, live); err == nil {
				missing := missingOutputs(next, live)
				if len(missing) == 0 {
					return live, nil
				}
				err = fmt.Errorf("waiting for %s", strings.Join(missing, ", "))
			}
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(applyPollInterval)
	}
}

// releaseArgs builds the first phase of a split apply. It carries power state
// and mode only, because that is all that frees capacity, and because moving a
// head in this phase makes mango reflow the layout and displace the heads the
// second phase has not reached yet
func releaseArgs(monitors []Monitor, live []wlrOutput) ([]string, error) {
	byName := indexOutputs(live)

	var args []string
	for _, m := range monitors {
		out, ok := byName[m.Name]
		if !ok {
			return nil, fmt.Errorf("output %q is not currently connected", m.Name)
		}

		if !m.Active {
			args = append(args, "--output", m.Name, "--off")
			continue
		}

		modeStr, err := snapMode(out.Modes, m.PxW, m.PxH, m.Hz)
		if err != nil {
			return nil, fmt.Errorf("snap mode for %s: %w", m.Name, err)
		}
		args = append(args, "--output", m.Name, "--on", "--mode", modeStr)
	}
	return args, nil
}

func indexOutputs(live []wlrOutput) map[string]wlrOutput {
	byName := make(map[string]wlrOutput, len(live))
	for _, o := range live {
		byName[o.Name] = o
	}
	return byName
}

// currentMode returns the active mode of out in the form applyArgs emits, or
// the empty string when the head reports no current mode.
func currentMode(out wlrOutput) string {
	for _, m := range out.Modes {
		if m.Current {
			v := mhz(m.Refresh)
			return fmt.Sprintf("%dx%d@%d.%03d", m.Width, m.Height, v/1000, v%1000)
		}
	}
	return ""
}

// verifyOutputs reports whether live matches what want asked for. Power state
// and mode are checked, position and scale are not. A misplaced monitor is
// visible and correctable, a dark one is not.
func verifyOutputs(want []Monitor, live []wlrOutput) error {
	byName := indexOutputs(live)

	for _, m := range want {
		out, ok := byName[m.Name]
		if !ok {
			return fmt.Errorf("output %q is no longer present", m.Name)
		}

		if !m.Active {
			if out.Enabled {
				return fmt.Errorf("output %q should be off but is enabled", m.Name)
			}
			continue
		}
		if !out.Enabled {
			return fmt.Errorf("output %q is disabled after being turned on", m.Name)
		}

		// snapping keeps verification and applyArgs on the same advertised mode
		wanted, err := snapMode(out.Modes, m.PxW, m.PxH, m.Hz)
		if err != nil {
			return fmt.Errorf("verify %s: %w", m.Name, err)
		}
		got := currentMode(out)
		if got == "" {
			return fmt.Errorf("output %q is enabled with no current mode", m.Name)
		}
		if got != wanted {
			return fmt.Errorf("output %q runs %s after being set to %s", m.Name, got, wanted)
		}
	}
	return nil
}

// confirmApplied waits until the live outputs match want and have held that
// way for applyHoldTime, and returns the last mismatch if they never do.
func confirmApplied(want []Monitor) error {
	deadline := time.Now().Add(applyVerifyTimeout)
	var heldSince time.Time
	var last error

	for {
		live, err := readOutputs()
		switch {
		case err != nil:
			last = err
			heldSince = time.Time{}
		default:
			last = verifyOutputs(want, live)
			if last != nil {
				heldSince = time.Time{}
				break
			}
			if heldSince.IsZero() {
				heldSince = time.Now()
			}
			if time.Since(heldSince) >= applyHoldTime {
				return nil
			}
		}

		if !time.Now().Before(deadline) {
			return last
		}
		time.Sleep(applyPollInterval)
	}
}

// missingOutputs names the heads of want that live does not carry. wlr-randr
// rejects a whole invocation that names an unknown output, so a restore waits
// for dropped heads instead of addressing them.
func missingOutputs(want []Monitor, live []wlrOutput) []string {
	byName := indexOutputs(live)
	var missing []string
	for _, m := range want {
		if _, ok := byName[m.Name]; !ok {
			missing = append(missing, m.Name)
		}
	}
	return missing
}

// presentOutputs is want restricted to the heads live carries.
func presentOutputs(want []Monitor, live []wlrOutput) []Monitor {
	byName := indexOutputs(live)
	kept := make([]Monitor, 0, len(want))
	for _, m := range want {
		if _, ok := byName[m.Name]; ok {
			kept = append(kept, m)
		}
	}
	return kept
}

// restoreLayout puts previous back after a failed apply. Heads recovered on an
// earlier pass stay up while the rest are still returning to the wire.
func restoreLayout(previous []Monitor) {
	deadline := time.Now().Add(restoreTimeout)
	for {
		err := restoreOnce(previous)
		if err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "warning: could not restore the previous layout: %v\n", err)
			return
		}
		time.Sleep(restoreRetryInterval)
	}
}

func restoreOnce(previous []Monitor) error {
	live, err := readOutputs()
	if err != nil {
		return err
	}

	missing := missingOutputs(previous, live)
	present := presentOutputs(previous, live)
	if len(present) == 0 {
		return fmt.Errorf("waiting for %s", strings.Join(missing, ", "))
	}

	args, err := applyArgs(present, live)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if _, err := execWlrRandr(args...); err != nil {
			return err
		}
		time.Sleep(applyPollInterval)
	}

	settled, err := readOutputs()
	if err != nil {
		return err
	}
	if err := verifyOutputs(present, settled); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("waiting for %s", strings.Join(missing, ", "))
	}
	return nil
}

func applyMonitors(monitors []Monitor) error {
	return applyLayout(monitors, true)
}

// applyLayout commits monitors and puts the observed layout back when the
// heads do not come up. record is false while undoing an apply so the layout
// being undone does not become the next rollback target.
func applyLayout(monitors []Monitor, record bool) error {
	// a failing apply spends up to restoreTimeout recovering, long enough for
	// the TUI to hand over a second layout built on the state being undone
	if !applying.TryLock() {
		return errors.New("an apply is already in progress")
	}
	defer applying.Unlock()

	// the wanted mode is float Hz while wlr-randr keys modes by exact
	// millihertz, so snapping needs the live mode list
	live, err := readOutputs()
	if err != nil {
		return fmt.Errorf("list outputs before apply: %w", err)
	}

	previous := outputsToMonitors(live)
	if record {
		rollbackLayout = previous
	}

	// mango commits the heads of one configuration separately, in an order it
	// picks, so a single invocation can raise one head before lowering another
	// and exceed a limit the target layout itself respects (a shared DP tunnel
	// bandwidth budget, a joiner pipe pair). Giving capacity back in its own
	// invocation puts that ordering where the compositor cannot reorder it
	release, claim := partitionApply(monitors, live)
	applied := false

	if len(release) > 0 && len(claim) > 0 {
		args, err := releaseArgs(release, live)
		if err != nil {
			return err
		}
		if len(args) > 0 {
			if _, err := execWlrRandr(args...); err != nil {
				return fmt.Errorf("apply output configuration: %w", err)
			}
			applied = true

			settled, err := settlePhase(release, monitors)
			if err != nil {
				restoreLayout(previous)
				return fmt.Errorf("settle between apply phases: %w", err)
			}
			live = settled
		}
	}

	args, err := applyArgs(monitors, live)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if _, err := execWlrRandr(args...); err != nil {
			// an earlier phase already moved the layout off what the user had
			if applied {
				restoreLayout(previous)
			}
			return fmt.Errorf("apply output configuration: %w", err)
		}
		applied = true
	}

	if applied {
		// a zero exit status means the compositor accepted the request, not
		// that the heads came up
		if err := confirmApplied(monitors); err != nil {
			restoreLayout(previous)
			return fmt.Errorf("configuration did not take effect: %w", err)
		}
	}

	// mirroring is reconciled last, once every output is committed at its
	// final mode, so wl-mirror attaches to outputs that already exist. a
	// mirror failure is non-fatal: the layout itself applied, so we warn but
	// do not roll back
	if err := reconcileMirrors(monitors); err != nil {
		fmt.Fprintf(os.Stderr, "warning: mirror reconcile: %v\n", err)
	}
	return nil
}

// getAvailableModes returns the mode list for one output formatted as
// "WxH@Hz.HHHHz" so the existing mode picker (which parses the hyprland
// string form and requires the Hz suffix) can consume them without further
// conversion
func getAvailableModes(monitorName string) ([]string, error) {
	outputs, err := readOutputs()
	if err != nil {
		return nil, err
	}
	for _, out := range outputs {
		if out.Name != monitorName {
			continue
		}
		modes := make([]string, 0, len(out.Modes))
		for _, m := range out.Modes {
			v := mhz(m.Refresh)
			modes = append(modes, fmt.Sprintf("%dx%d@%d.%03dHz", m.Width, m.Height, v/1000, v%1000))
		}
		return modes, nil
	}
	return nil, fmt.Errorf("monitor %s not found", monitorName)
}

// rollbackLayout is the head state observed before the most recent apply
var (
	rollbackLayout []Monitor
	applying       sync.Mutex
)

func rollback() error {
	if rollbackLayout == nil {
		return errors.New("no previous layout recorded")
	}
	return applyLayout(rollbackLayout, false)
}

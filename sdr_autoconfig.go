package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultSDRSysfsRoot    = "/sys/bus/usb/devices"
	defaultSDRStatePath    = "/app/config/sdr-autoconfig.json"
	defaultRadiodConfig    = "/etc/ka9q-radio/radiod@ubersdr.conf"
	sdrGeneratedConfigMark = "# Managed by UltraSDR hardware auto-configuration."
)

// USBDevice is the stable subset of Linux USB sysfs metadata needed to choose
// an SDR profile. No shell utilities or host udev database are required.
type USBDevice struct {
	Path         string `json:"path"`
	VendorID     string `json:"vendor_id"`
	ProductID    string `json:"product_id"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Serial       string `json:"serial,omitempty"`
}

func (d USBDevice) Fingerprint() string {
	serial := strings.ToLower(strings.TrimSpace(d.Serial))
	if serial == "" {
		serial = filepath.Base(d.Path)
	}
	return strings.ToLower(d.VendorID + ":" + d.ProductID + ":" + serial)
}

type SDRUSBMatch struct {
	VendorID       string
	ProductIDs     []string
	ProductContains []string
}

// SDRDriverIntegration describes how a radio module becomes available. Native
// modules ship in the radio image. Bundle modules may be installed from a
// vendor-authorized package without granting the Web UI a package manager or
// Docker socket.
type SDRDriverIntegration struct {
	ID                   string `json:"id"`
	DisplayName          string `json:"display_name"`
	Status               string `json:"status"`
	License               string `json:"license"`
	Redistribution       string `json:"redistribution"`
	OfficialURL          string `json:"official_url,omitempty"`
	BundleInstallSupported bool `json:"bundle_install_supported"`
	Notes                string `json:"notes,omitempty"`
}

type SDRAutoProfile struct {
	ID               string            `json:"id"`
	DisplayName      string            `json:"display_name"`
	Driver           string            `json:"driver"`
	Integration      string            `json:"integration"`
	Priority         int               `json:"priority"`
	Matches          []SDRUSBMatch     `json:"-"`
	Receiver         ReceiverConfig    `json:"receiver"`
	DefaultFrequency uint64            `json:"default_frequency"`
	DefaultMode      string            `json:"default_mode"`
	Options          map[string]string `json:"options,omitempty"`
	Notes            string            `json:"notes,omitempty"`
}

type SDRCandidate struct {
	Profile SDRAutoProfile `json:"profile"`
	Device  USBDevice      `json:"device"`
}

type SDRAutoConfigStatus struct {
	SchemaVersion int                    `json:"schema_version"`
	UpdatedAt     time.Time              `json:"updated_at"`
	State         string                 `json:"state"`
	Message       string                 `json:"message"`
	Managed       bool                   `json:"managed"`
	Selected      *SDRCandidate          `json:"selected,omitempty"`
	Detected      []USBDevice            `json:"detected"`
	Candidates    []SDRCandidate         `json:"candidates"`
	Integrations  []SDRDriverIntegration `json:"integrations"`
}

type SDRAutoConfigOptions struct {
	SysfsRoot        string
	UberSDRConfig    string
	RadiodConfig     string
	StatePath        string
	DriverRoot       string
	RequestedProfile string
	RequestedSerial  string
	Force            bool
	DryRun           bool
}

func builtinSDRIntegrations() []SDRDriverIntegration {
	return []SDRDriverIntegration{
		{ID: "native", DisplayName: "Built into the UltraSDR radio image", Status: "ready", License: "Open-source components", Redistribution: "included", BundleInstallSupported: false},
		{ID: "sdrplay", DisplayName: "SDRplay API", Status: "vendor-package-required", License: "SDRplay API license", Redistribution: "vendor-controlled", OfficialURL: "https://www.sdrplay.com/api/", BundleInstallSupported: true, Notes: "Download the official Linux API package, accept its license, then upload the UltraSDR driver bundle."},
		{ID: "vendor-bundle", DisplayName: "Vendor driver bundle", Status: "bundle-required", License: "Varies by vendor", Redistribution: "not bundled", BundleInstallSupported: true, Notes: "UltraSDR installs declarative native-library bundles only; install scripts are never executed."},
		{ID: "external-radiod", DisplayName: "External KA9Q-compatible bridge", Status: "bridge-required", License: "Varies", Redistribution: "not bundled", BundleInstallSupported: false, Notes: "For devices served by SoapySDR, UHD, libiio, network receivers, or another host."},
	}
}

func builtinSDRAutoProfiles() []SDRAutoProfile {
	profiles := []SDRAutoProfile{
		{
			ID: "rx888", DisplayName: "RX-888 family", Driver: "rx888", Integration: "native", Priority: 100,
			Matches: []SDRUSBMatch{{VendorID: "04b4", ProductIDs: []string{"00f1", "00f3", "00bc", "00b0", "0053"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "rx888", Device: "rx888", Description: "RX-888 wideband HF receiver", SampleRate: 64_800_000, FrequencyMinHz: 10_000, FrequencyMaxHz: 30_000_000},
			DefaultFrequency: 14_175_000, DefaultMode: "usb",
			Options: map[string]string{"gainmode": "high", "gain": "10", "att": "0", "calibrate": "0"},
			Notes: "Wideband HF defaults; Bias-T remains off for antenna safety.",
		},
		{
			ID: "airspy-hf", DisplayName: "Airspy HF+ family", Driver: "airspyhf", Integration: "native", Priority: 95,
			Matches: []SDRUSBMatch{{VendorID: "03eb", ProductIDs: []string{"800c"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "airspyhf", Device: "airspyhf", Description: "Airspy HF+ receiver", SampleRate: 768_000, CenterFrequency: 14_175_000, FrequencyMinHz: 9_000, FrequencyMaxHz: 260_000_000},
			DefaultFrequency: 14_175_000, DefaultMode: "usb",
			Options: map[string]string{"hf-agc": "yes", "lib-dsp": "yes"},
			Notes: "Uses KA9Q's stable 768 ksample/s setting; Bias-T remains off.",
		},
		{
			ID: "airspy", DisplayName: "Airspy R2 / Mini", Driver: "airspy", Integration: "native", Priority: 90,
			Matches: []SDRUSBMatch{{VendorID: "1d50", ProductIDs: []string{"60a1"}, ProductContains: []string{"airspy"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "airspy", Device: "airspy", Description: "Airspy receiver", SampleRate: 2_500_000, CenterFrequency: 145_000_000, FrequencyMinHz: 24_000_000, FrequencyMaxHz: 1_800_000_000},
			DefaultFrequency: 145_500_000, DefaultMode: "nfm",
			Options: map[string]string{"lna-agc": "true", "mixer-agc": "true", "agc-low-threshold": "-70", "bias": "false"},
			Notes: "Conservative R2-compatible sample rate. Mini owners can select the 3/6 MSPS presets after detection.",
		},
		{
			ID: "rtlsdr", DisplayName: "RTL-SDR compatible", Driver: "rtlsdr", Integration: "native", Priority: 80,
			Matches: []SDRUSBMatch{
				{VendorID: "0bda", ProductIDs: []string{"2832", "2838"}},
				{VendorID: "0413", ProductIDs: []string{"6680", "6f0f"}},
				{VendorID: "0458", ProductIDs: []string{"707f"}},
				{VendorID: "0ccd"},
				{VendorID: "15f4", ProductIDs: []string{"0131", "0133"}},
				{VendorID: "1b80"},
				{VendorID: "1d19", ProductIDs: []string{"1101", "1102", "1103", "1104"}},
				{VendorID: "1f4d"},
			},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "rtlsdr", Device: "rtlsdr", Description: "RTL-SDR receiver", SampleRate: 1_800_000, CenterFrequency: 145_000_000, FrequencyMinHz: 24_000_000, FrequencyMaxHz: 1_766_000_000},
			DefaultFrequency: 145_500_000, DefaultMode: "nfm",
			Options: map[string]string{"agc": "true", "gain": "0", "bias": "false", "direct_sampling": "0"},
			Notes: "KA9Q's reliable 1.8 MSPS default. Bias-T and direct sampling are deliberately off.",
		},
		{
			ID: "fobos", DisplayName: "RigExpert Fobos SDR", Driver: "fobos", Integration: "vendor-bundle", Priority: 88,
			Matches: []SDRUSBMatch{{VendorID: "16d0", ProductIDs: []string{"132e"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "fobos", Device: "fobos", Description: "Fobos SDR receiver", SampleRate: 50_000_000, CenterFrequency: 162_480_000, FrequencyMinHz: 50_000, FrequencyMaxHz: 6_000_000_000},
			DefaultFrequency: 162_550_000, DefaultMode: "nfm",
			Options: map[string]string{"direct_sampling": "no", "lna_gain": "2", "vga_gain": "15", "clk_source": "0", "hf_input": "0"},
		},
		{
			ID: "funcube-pro-plus", DisplayName: "FUNcube Dongle Pro+", Driver: "funcube", Integration: "native", Priority: 82,
			Matches: []SDRUSBMatch{{VendorID: "04d8", ProductIDs: []string{"fb31"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "funcube", Device: "funcube", Description: "FUNcube Dongle Pro+ receiver", SampleRate: 192_000, CenterFrequency: 145_000_000, FrequencyMinHz: 150_000, FrequencyMaxHz: 1_900_000_000},
			DefaultFrequency: 145_500_000, DefaultMode: "nfm",
		},
		{
			ID: "funcube-pro", DisplayName: "FUNcube Dongle Pro", Driver: "funcube", Integration: "native", Priority: 81,
			Matches: []SDRUSBMatch{{VendorID: "04d8", ProductIDs: []string{"fb56"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "funcube", Device: "funcube", Description: "FUNcube Dongle Pro receiver", SampleRate: 96_000, CenterFrequency: 145_000_000, FrequencyMinHz: 64_000_000, FrequencyMaxHz: 1_700_000_000},
			DefaultFrequency: 145_500_000, DefaultMode: "nfm",
		},
		{
			ID: "hackrf", DisplayName: "HackRF family", Driver: "hackrf", Integration: "native", Priority: 85,
			Matches: []SDRUSBMatch{{VendorID: "1d50", ProductIDs: []string{"6089"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "hackrf", Device: "hackrf", Description: "HackRF receiver", SampleRate: 10_000_000, CenterFrequency: 100_000_000, FrequencyMinHz: 1_000_000, FrequencyMaxHz: 6_000_000_000},
			DefaultFrequency: 100_000_000, DefaultMode: "wfm",
			Options: map[string]string{"lna-gain": "16", "mixer-gain": "16", "if-gain": "16"},
		},
		{
			ID: "sdrplay", DisplayName: "SDRplay RSP family", Driver: "sdrplay", Integration: "sdrplay", Priority: 92,
			Matches: []SDRUSBMatch{{VendorID: "1df7"}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "sdrplay", Device: "sdrplay", Description: "SDRplay RSP receiver", SampleRate: 2_000_000, CenterFrequency: 100_000_000, FrequencyMinHz: 1_000, FrequencyMaxHz: 2_000_000_000},
			DefaultFrequency: 100_000_000, DefaultMode: "wfm",
			Options: map[string]string{"if-agc": "yes", "lna-state": "2", "bias-t": "no"},
			Notes: "Requires the official SDRplay API and a matching radiod module bundle.",
		},
		{
			ID: "bladerf", DisplayName: "Nuand bladeRF family", Driver: "bladerf", Integration: "native", Priority: 86,
			Matches: []SDRUSBMatch{{VendorID: "2cf0", ProductIDs: []string{"5246", "5250"}}, {VendorID: "1d50", ProductIDs: []string{"6066"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "bladerf", Device: "bladerf", Description: "bladeRF receiver", SampleRate: 10_000_000, CenterFrequency: 100_000_000, FrequencyMinHz: 47_000_000, FrequencyMaxHz: 6_000_000_000},
			DefaultFrequency: 100_000_000, DefaultMode: "wfm",
			Options: map[string]string{"bandwidth": "10000000", "gain": "30", "bias": "false"},
		},
		{
			ID: "hydrasdr", DisplayName: "HydraSDR RFOne", Driver: "hydrasdr", Integration: "vendor-bundle", Priority: 91,
			Matches: []SDRUSBMatch{{ProductContains: []string{"hydrasdr", "rfone"}}},
			Receiver: ReceiverConfig{Backend: "ka9q-radiod", Driver: "hydrasdr", Device: "hydrasdr", Description: "HydraSDR receiver", SampleRate: 10_000_000, CenterFrequency: 100_000_000, FrequencyMinHz: 24_000_000, FrequencyMaxHz: 1_800_000_000},
			DefaultFrequency: 100_000_000, DefaultMode: "wfm",
			Options: map[string]string{"linearity": "false", "bias": "false", "agc-low-threshold": "-70"},
		},
	}
	for i := range profiles {
		profiles[i].Receiver.Options = cloneStringMap(profiles[i].Options)
	}
	return profiles
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func normalizeUSBID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func DetectUSBDevices(root string) ([]USBDevice, error) {
	if root == "" {
		root = defaultSDRSysfsRoot
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read USB sysfs %s: %w", root, err)
	}
	devices := make([]USBDevice, 0)
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		path := filepath.Join(root, entry.Name())
		vendor := normalizeUSBID(readTrimmedFile(filepath.Join(path, "idVendor")))
		product := normalizeUSBID(readTrimmedFile(filepath.Join(path, "idProduct")))
		if vendor == "" || product == "" {
			continue
		}
		devices = append(devices, USBDevice{
			Path:         path,
			VendorID:     vendor,
			ProductID:    product,
			Manufacturer: readTrimmedFile(filepath.Join(path, "manufacturer")),
			Product:      readTrimmedFile(filepath.Join(path, "product")),
			Serial:       readTrimmedFile(filepath.Join(path, "serial")),
		})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Fingerprint() < devices[j].Fingerprint() })
	return devices, nil
}

func usbMatch(match SDRUSBMatch, device USBDevice) bool {
	if match.VendorID != "" && normalizeUSBID(match.VendorID) != device.VendorID {
		return false
	}
	if len(match.ProductIDs) > 0 {
		found := false
		for _, product := range match.ProductIDs {
			if normalizeUSBID(product) == device.ProductID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(match.ProductContains) > 0 {
		haystack := strings.ToLower(device.Manufacturer + " " + device.Product)
		found := false
		for _, fragment := range match.ProductContains {
			if strings.Contains(haystack, strings.ToLower(fragment)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return match.VendorID != "" || len(match.ProductIDs) > 0 || len(match.ProductContains) > 0
}

func FindSDRCandidates(devices []USBDevice, profiles []SDRAutoProfile) []SDRCandidate {
	var candidates []SDRCandidate
	for _, device := range devices {
		for _, profile := range profiles {
			for _, match := range profile.Matches {
				if usbMatch(match, device) {
					candidate := SDRCandidate{Profile: profile, Device: device}
					if device.Serial != "" {
						candidate.Profile.Receiver.Serial = device.Serial
					}
					candidates = append(candidates, candidate)
					break
				}
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Profile.Priority != candidates[j].Profile.Priority {
			return candidates[i].Profile.Priority > candidates[j].Profile.Priority
		}
		return candidates[i].Device.Fingerprint() < candidates[j].Device.Fingerprint()
	})
	return candidates
}

func selectSDRCandidate(candidates []SDRCandidate, profileID, serial string) (*SDRCandidate, string) {
	profileID = strings.ToLower(strings.TrimSpace(profileID))
	serial = strings.ToLower(strings.TrimSpace(serial))
	filtered := make([]SDRCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if profileID != "" && strings.ToLower(candidate.Profile.ID) != profileID {
			continue
		}
		if serial != "" && strings.ToLower(candidate.Device.Serial) != serial {
			continue
		}
		filtered = append(filtered, candidate)
	}
	if len(filtered) == 1 {
		selected := filtered[0]
		return &selected, ""
	}
	if len(filtered) == 0 {
		if profileID != "" || serial != "" {
			return nil, "the requested SDR profile or serial is not attached"
		}
		return nil, "no supported USB SDR was detected"
	}
	if profileID == "" && serial == "" {
		return nil, "multiple supported SDRs are attached; select one in Hardware Setup or set SDR_PROFILE/SDR_SERIAL"
	}
	return nil, "the selection matches multiple SDRs; specify a unique serial"
}

func integrationByID(id string) (SDRDriverIntegration, bool) {
	for _, integration := range builtinSDRIntegrations() {
		if integration.ID == id {
			return integration, true
		}
	}
	return SDRDriverIntegration{}, false
}

func PlanSDRAutoConfig(devices []USBDevice, requestedProfile, requestedSerial string) SDRAutoConfigStatus {
	candidates := FindSDRCandidates(devices, builtinSDRAutoProfiles())
	selected, reason := selectSDRCandidate(candidates, requestedProfile, requestedSerial)
	status := SDRAutoConfigStatus{
		SchemaVersion: 1,
		UpdatedAt:     time.Now().UTC(),
		State:         "needs-hardware",
		Message:       reason,
		Detected:      devices,
		Candidates:    candidates,
		Integrations:  builtinSDRIntegrations(),
	}
	if selected == nil {
		if len(candidates) > 1 {
			status.State = "needs-selection"
		}
		return status
	}
	status.Selected = selected
	integration, _ := integrationByID(selected.Profile.Integration)
	if integration.Status != "ready" {
		status.State = "needs-driver"
		status.Message = integration.Notes
		return status
	}
	status.State = "ready"
	status.Message = selected.Profile.DisplayName + " detected and ready"
	return status
}

func RunSDRAutoConfig(options SDRAutoConfigOptions) (SDRAutoConfigStatus, error) {
	if options.SysfsRoot == "" {
		options.SysfsRoot = defaultSDRSysfsRoot
	}
	if options.RadiodConfig == "" {
		options.RadiodConfig = defaultRadiodConfig
	}
	if options.StatePath == "" {
		options.StatePath = defaultSDRStatePath
	}
	if options.DriverRoot == "" {
		options.DriverRoot = defaultSDRDriverRoot
	}
	devices, err := DetectUSBDevices(options.SysfsRoot)
	if err != nil {
		return SDRAutoConfigStatus{}, err
	}
	status := PlanSDRAutoConfig(devices, options.RequestedProfile, options.RequestedSerial)
	resolveInstalledSDRDrivers(&status, options.DriverRoot)
	if status.State != "ready" {
		// Keep the management UI alive when no usable hardware is ready. A
		// synthetic frontend avoids a radiod crash loop and is replaced on the
		// next scan/apply. Never overwrite a hand-written receiver config.
		if options.Force || isManagedRadiodConfig(options.RadiodConfig) {
			if err := writeSetupReceiverConfig(options, &status); err != nil {
				return status, err
			}
		} else if _, err := os.Stat(options.RadiodConfig); errors.Is(err, fs.ErrNotExist) {
			if err := writeSetupReceiverConfig(options, &status); err != nil {
				return status, err
			}
		}
		if err := writeSDRAutoConfigState(options.StatePath, status, options.DryRun); err != nil {
			return status, err
		}
		return status, nil
	}

	if !options.Force && !isManagedRadiodConfig(options.RadiodConfig) {
		if _, err := os.Stat(options.RadiodConfig); err == nil {
			status.State = "manual-config-preserved"
			status.Message = "Existing hand-written radiod configuration preserved; choose Apply in Hardware Setup to replace it"
			if err := writeSDRAutoConfigState(options.StatePath, status, options.DryRun); err != nil {
				return status, err
			}
			return status, nil
		}
	}

	configText, err := BuildRadiodConfig(status.Selected.Profile.Receiver, RadiodConfig{
		StatusGroup: "hf-status.local:5006",
		DataGroup:   "pcm.local:5004",
	})
	if err != nil {
		return status, err
	}
	configText = sdrGeneratedConfigMark + "\n" + configText
	if !options.DryRun {
		if err := os.MkdirAll(filepath.Dir(options.RadiodConfig), 0755); err != nil {
			return status, err
		}
		if err := atomicWriteFile(options.RadiodConfig, []byte(configText)); err != nil {
			return status, fmt.Errorf("write radiod configuration: %w", err)
		}
		if options.UberSDRConfig != "" {
			if err := updateUberSDRReceiverConfig(options.UberSDRConfig, *status.Selected); err != nil {
				return status, err
			}
		}
	}
	status.Managed = true
	status.Message = status.Selected.Profile.DisplayName + " configured with safe optimized defaults"
	if err := writeSDRAutoConfigState(options.StatePath, status, options.DryRun); err != nil {
		return status, err
	}
	return status, nil
}

func writeSetupReceiverConfig(options SDRAutoConfigOptions, status *SDRAutoConfigStatus) error {
	fallback := ReceiverConfig{
		Backend:         "ka9q-radiod",
		Driver:          "sig_gen",
		Device:          "sig_gen",
		Description:     "UltraSDR hardware setup receiver",
		SampleRate:      30_000_000,
		FrequencyMinHz:  10_000,
		FrequencyMaxHz:  15_000_000,
		Options: map[string]string{
			"carrier":    "10m0",
			"amplitude":  "-40",
			"noise":      "-80",
			"real":       "true",
			"modulation": "CW",
		},
	}
	configText, err := BuildRadiodConfig(fallback, RadiodConfig{
		StatusGroup: "hf-status.local:5006",
		DataGroup:   "pcm.local:5004",
	})
	if err != nil {
		return err
	}
	if !options.DryRun {
		if err := os.MkdirAll(filepath.Dir(options.RadiodConfig), 0755); err != nil {
			return err
		}
		if err := atomicWriteFile(options.RadiodConfig, []byte(sdrGeneratedConfigMark+"\n"+configText)); err != nil {
			return fmt.Errorf("write setup receiver configuration: %w", err)
		}
	}
	status.Managed = true
	if status.Message != "" {
		status.Message += ". "
	}
	status.Message += "The setup receiver is active so the Web UI remains available"
	return nil
}

func isManagedRadiodConfig(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), sdrGeneratedConfigMark)
}

func writeSDRAutoConfigState(path string, status SDRAutoConfigStatus, dryRun bool) error {
	if dryRun || path == "" {
		return nil
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicWriteFile(path, data)
}

func updateUberSDRReceiverConfig(path string, selected SDRCandidate) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read UltraSDR config: %w", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse UltraSDR config: %w", err)
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("UltraSDR config root must be a YAML mapping")
	}
	receiverNode, err := valueToYAMLNode(selected.Profile.Receiver)
	if err != nil {
		return err
	}
	setYAMLMapping(root.Content[0], "receiver", receiverNode)
	admin := ensureYAMLMapping(root.Content[0], "admin")
	setYAMLMapping(admin, "default_frequency", scalarYAMLNode(strconv.FormatUint(selected.Profile.DefaultFrequency, 10), "!!int"))
	setYAMLMapping(admin, "default_mode", scalarYAMLNode(selected.Profile.DefaultMode, "!!str"))

	output, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("encode UltraSDR config: %w", err)
	}
	if err := atomicWriteFile(path, output); err != nil {
		return fmt.Errorf("write UltraSDR config: %w", err)
	}
	return nil
}

func valueToYAMLNode(value any) (*yaml.Node, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 {
		return nil, fmt.Errorf("cannot encode YAML value")
	}
	return document.Content[0], nil
}

func setYAMLMapping(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, scalarYAMLNode(key, "!!str"), value)
}

func ensureYAMLMapping(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key && mapping.Content[i+1].Kind == yaml.MappingNode {
			return mapping.Content[i+1]
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setYAMLMapping(mapping, key, child)
	return child
}

func scalarYAMLNode(value, tag string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func runSDRAutoconfigureCommand(args []string) int {
	options := SDRAutoConfigOptions{
		SysfsRoot:        getenvDefault("SDR_SYSFS_ROOT", defaultSDRSysfsRoot),
		UberSDRConfig:    getenvDefault("SDR_UBERSDR_CONFIG", "/app/config/config.yaml"),
		RadiodConfig:     getenvDefault("SDR_RADIOD_CONFIG", defaultRadiodConfig),
		StatePath:        getenvDefault("SDR_STATE_PATH", defaultSDRStatePath),
		DriverRoot:       getenvDefault("SDR_DRIVER_ROOT", defaultSDRDriverRoot),
		RequestedProfile: os.Getenv("SDR_PROFILE"),
		RequestedSerial:  os.Getenv("SDR_SERIAL"),
		Force:            envBool("SDR_AUTOCONFIG_FORCE"),
		DryRun:           envBool("SDR_AUTOCONFIG_DRY_RUN"),
	}
	for _, arg := range args {
		if arg == "--force" {
			options.Force = true
		}
		if arg == "--dry-run" {
			options.DryRun = true
		}
	}
	status, err := RunSDRAutoConfig(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SDR auto-configuration failed: %v\n", err)
		return 1
	}
	encoded, _ := json.MarshalIndent(status, "", "  ")
	fmt.Println(string(encoded))
	switch status.State {
	case "ready", "manual-config-preserved", "needs-hardware", "needs-selection", "needs-driver":
		return 0
	default:
		return 1
	}
}

func getenvDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func currentArchitecture() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

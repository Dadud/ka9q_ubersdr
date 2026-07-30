package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUSBFixture(t *testing.T, root, name, vendor, product, manufacturer, model, serial string) {
	t.Helper()
	deviceRoot := filepath.Join(root, name)
	if err := os.MkdirAll(deviceRoot, 0755); err != nil {
		t.Fatal(err)
	}
	for filename, value := range map[string]string{
		"idVendor": vendor, "idProduct": product, "manufacturer": manufacturer,
		"product": model, "serial": serial,
	} {
		if value == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(deviceRoot, filename), []byte(value+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectAndPlanRX888(t *testing.T) {
	root := t.TempDir()
	writeUSBFixture(t, root, "1-2", "04B4", "00F1", "Cypress", "RX888 MKII", "RX123")
	devices, err := DetectUSBDevices(root)
	if err != nil {
		t.Fatal(err)
	}
	status := PlanSDRAutoConfig(devices, "", "")
	if status.State != "ready" || status.Selected == nil {
		t.Fatalf("unexpected status: %#v", status)
	}
	if status.Selected.Profile.ID != "rx888" || status.Selected.Profile.Receiver.Serial != "RX123" {
		t.Fatalf("wrong selection: %#v", status.Selected)
	}
	if status.Selected.Profile.Receiver.Options["bias-hf"] != "" {
		t.Fatal("auto-configuration must never enable Bias-T")
	}
}

func TestMultipleReceiversRequireSelection(t *testing.T) {
	root := t.TempDir()
	writeUSBFixture(t, root, "1-2", "04b4", "00f1", "Cypress", "RX888", "RX1")
	writeUSBFixture(t, root, "1-3", "0bda", "2838", "RTLSDRBlog", "Blog V4", "RTL1")
	devices, err := DetectUSBDevices(root)
	if err != nil {
		t.Fatal(err)
	}
	status := PlanSDRAutoConfig(devices, "", "")
	if status.State != "needs-selection" {
		t.Fatalf("got %q, want needs-selection", status.State)
	}
	status = PlanSDRAutoConfig(devices, "rtlsdr", "RTL1")
	if status.State != "ready" || status.Selected.Profile.ID != "rtlsdr" {
		t.Fatalf("explicit selection failed: %#v", status)
	}
}

func TestRunSDRAutoConfigWritesManagedConfigs(t *testing.T) {
	root := t.TempDir()
	sysfs := filepath.Join(root, "usb")
	if err := os.MkdirAll(sysfs, 0755); err != nil {
		t.Fatal(err)
	}
	writeUSBFixture(t, sysfs, "2-1", "0bda", "2838", "RTLSDRBlog", "Blog V4", "00000001")
	uberConfig := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(uberConfig, []byte("admin:\n  default_frequency: 10000000\n  default_mode: usb\nradiod:\n  status_group: hf-status.local:5006\n  data_group: pcm.local:5004\n"), 0644); err != nil {
		t.Fatal(err)
	}
	radiodConfig := filepath.Join(root, "radiod@ubersdr.conf")
	status, err := RunSDRAutoConfig(SDRAutoConfigOptions{
		SysfsRoot:     sysfs,
		UberSDRConfig: uberConfig,
		RadiodConfig:  radiodConfig,
		StatePath:     filepath.Join(root, "state.json"),
		DriverRoot:    filepath.Join(root, "drivers"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || !status.Managed {
		t.Fatalf("unexpected status: %#v", status)
	}
	radiodData, err := os.ReadFile(radiodConfig)
	if err != nil {
		t.Fatal(err)
	}
	text := string(radiodData)
	for _, fragment := range []string{sdrGeneratedConfigMark, "hardware = rtlsdr", "samprate = 1800000", "serial = \"00000001\"", "bias = false"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("radiod config missing %q:\n%s", fragment, text)
		}
	}
	config, err := LoadConfig(uberConfig)
	if err != nil {
		t.Fatal(err)
	}
	if config.Receiver.Driver != "rtlsdr" || config.Admin.DefaultMode != "nfm" {
		t.Fatalf("UltraSDR config was not updated: %#v", config.Receiver)
	}
}

func TestNoHardwareCreatesSetupReceiver(t *testing.T) {
	root := t.TempDir()
	sysfs := filepath.Join(root, "usb")
	if err := os.MkdirAll(sysfs, 0755); err != nil {
		t.Fatal(err)
	}
	status, err := RunSDRAutoConfig(SDRAutoConfigOptions{
		SysfsRoot:    sysfs,
		RadiodConfig: filepath.Join(root, "radiod.conf"),
		StatePath:    filepath.Join(root, "state.json"),
		DriverRoot:   filepath.Join(root, "drivers"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "needs-hardware" || !status.Managed {
		t.Fatalf("unexpected setup state: %#v", status)
	}
	data, err := os.ReadFile(filepath.Join(root, "radiod.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hardware = sig_gen") {
		t.Fatalf("setup receiver not generated:\n%s", data)
	}
}

func makeDriverBundle(t *testing.T, integration, architecture string, files map[string][]byte) []byte {
	t.Helper()
	manifest := SDRDriverBundleManifest{
		SchemaVersion: 1, IntegrationID: integration, Version: "1.0.0",
		Architecture: architecture, LicenseName: "Test vendor license",
	}
	for name, data := range files {
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, SDRDriverBundleFile{Path: name, SHA256: hex.EncodeToString(sum[:])})
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, _ := writer.Create("manifest.json")
	_, _ = entry.Write(manifestData)
	for name, data := range files {
		entry, _ = writer.Create(name)
		_, _ = entry.Write(data)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestInstallDriverBundleAndResolve(t *testing.T) {
	root := t.TempDir()
	bundle := makeDriverBundle(t, "sdrplay", currentArchitecture(), map[string][]byte{
		"modules/sdrplay.so": []byte("test module"),
		"lib/libsdrplay_api.so.3": []byte("test library"),
	})
	manifest, err := InstallSDRDriverBundle(bytes.NewReader(bundle), int64(len(bundle)), root, true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.IntegrationID != "sdrplay" || !isDriverIntegrationInstalled(root, "sdrplay", "sdrplay") {
		t.Fatal("driver bundle was not activated")
	}

	status := PlanSDRAutoConfig([]USBDevice{{Path: "1-1", VendorID: "1df7", ProductID: "0001", Serial: "RSP1"}}, "", "")
	if status.State != "needs-driver" {
		t.Fatalf("got %q before install resolution", status.State)
	}
	resolveInstalledSDRDrivers(&status, root)
	if status.State != "ready" || status.Selected == nil {
		t.Fatalf("installed bundle was not resolved: %#v", status)
	}
	if !strings.Contains(status.Selected.Profile.Receiver.Options["library"], "sdrplay.so") {
		t.Fatal("installed module path was not added to receiver options")
	}
}

func TestDriverBundleRejectsLicenseAndTraversal(t *testing.T) {
	root := t.TempDir()
	valid := makeDriverBundle(t, "sdrplay", currentArchitecture(), map[string][]byte{
		"modules/sdrplay.so": []byte("test"),
	})
	if _, err := InstallSDRDriverBundle(bytes.NewReader(valid), int64(len(valid)), root, false); err == nil {
		t.Fatal("bundle installed without license acceptance")
	}
	bad := makeDriverBundle(t, "sdrplay", currentArchitecture(), map[string][]byte{
		"../sdrplay.so": []byte("test"),
	})
	if _, err := InstallSDRDriverBundle(bytes.NewReader(bad), int64(len(bad)), root, true); err == nil {
		t.Fatal("bundle with traversal path was accepted")
	}
}

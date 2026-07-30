package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultSDRDriverRoot      = "/opt/ultrasdr-drivers"
	maxSDRDriverBundleBytes   = int64(256 << 20)
	maxSDRDriverExpandedBytes = int64(512 << 20)
	maxSDRDriverFiles         = 256
)

type SDRDriverBundleFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type SDRDriverBundleManifest struct {
	SchemaVersion int                   `json:"schema_version"`
	IntegrationID string                `json:"integration_id"`
	Version       string                `json:"version"`
	Architecture  string                `json:"architecture"`
	LicenseName   string                `json:"license_name"`
	LicenseURL    string                `json:"license_url,omitempty"`
	Files         []SDRDriverBundleFile `json:"files"`
	InstalledAt   time.Time             `json:"installed_at,omitempty"`
}

func validDriverBundlePath(name string) bool {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean != name || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
		return false
	}
	if !(strings.HasPrefix(clean, "modules/") || strings.HasPrefix(clean, "lib/")) {
		return false
	}
	base := path.Base(clean)
	return strings.Contains(base, ".so") && !strings.ContainsAny(base, "\x00\r\n")
}

func isDriverIntegrationInstalled(root, integrationID, driver string) bool {
	if integrationID == "native" {
		return true
	}
	module := filepath.Join(root, integrationID, "modules", driver+".so")
	manifest := filepath.Join(root, integrationID, "manifest.json")
	return fileExists(module) && fileExists(manifest)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func resolveInstalledSDRDrivers(status *SDRAutoConfigStatus, root string) {
	for i := range status.Candidates {
		candidate := &status.Candidates[i]
		if isDriverIntegrationInstalled(root, candidate.Profile.Integration, candidate.Profile.Driver) {
			candidate.Profile.Receiver.Options = cloneStringMap(candidate.Profile.Receiver.Options)
			if candidate.Profile.Integration != "native" {
				candidate.Profile.Receiver.Options["library"] = filepath.ToSlash(filepath.Join(
					root, candidate.Profile.Integration, "modules", candidate.Profile.Driver+".so"))
			}
		}
	}
	if status.Selected == nil {
		return
	}
	for i := range status.Candidates {
		candidate := status.Candidates[i]
		if candidate.Profile.ID == status.Selected.Profile.ID &&
			candidate.Device.Fingerprint() == status.Selected.Device.Fingerprint() {
			status.Selected = &candidate
			break
		}
	}
	if isDriverIntegrationInstalled(root, status.Selected.Profile.Integration, status.Selected.Profile.Driver) {
		status.State = "ready"
		status.Message = status.Selected.Profile.DisplayName + " detected; installed driver bundle is ready"
	}
}

func InstallSDRDriverBundle(readerAt io.ReaderAt, size int64, root string, licenseAccepted bool) (SDRDriverBundleManifest, error) {
	if !licenseAccepted {
		return SDRDriverBundleManifest{}, fmt.Errorf("vendor license acceptance is required")
	}
	if size <= 0 || size > maxSDRDriverBundleBytes {
		return SDRDriverBundleManifest{}, fmt.Errorf("driver bundle must be between 1 byte and %d MiB", maxSDRDriverBundleBytes>>20)
	}
	archive, err := zip.NewReader(readerAt, size)
	if err != nil {
		return SDRDriverBundleManifest{}, fmt.Errorf("open driver bundle: %w", err)
	}
	if len(archive.File) == 0 || len(archive.File) > maxSDRDriverFiles+1 {
		return SDRDriverBundleManifest{}, fmt.Errorf("driver bundle contains an invalid number of files")
	}

	var manifest SDRDriverBundleManifest
	var manifestFound bool
	filesByName := make(map[string]*zip.File)
	var expanded int64
	for _, zipped := range archive.File {
		name := strings.ReplaceAll(zipped.Name, "\\", "/")
		if name == "manifest.json" {
			if zipped.UncompressedSize64 > 1<<20 {
				return manifest, fmt.Errorf("manifest is too large")
			}
			handle, err := zipped.Open()
			if err != nil {
				return manifest, err
			}
			data, err := io.ReadAll(io.LimitReader(handle, (1<<20)+1))
			_ = handle.Close()
			if err != nil {
				return manifest, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return manifest, fmt.Errorf("parse manifest: %w", err)
			}
			manifestFound = true
			continue
		}
		if !validDriverBundlePath(name) || zipped.FileInfo().IsDir() {
			return manifest, fmt.Errorf("bundle path %q is not an allowed shared-library path", zipped.Name)
		}
		expanded += int64(zipped.UncompressedSize64)
		if expanded > maxSDRDriverExpandedBytes {
			return manifest, fmt.Errorf("expanded driver bundle exceeds %d MiB", maxSDRDriverExpandedBytes>>20)
		}
		filesByName[name] = zipped
	}
	if !manifestFound {
		return manifest, fmt.Errorf("manifest.json is required")
	}
	if manifest.SchemaVersion != 1 {
		return manifest, fmt.Errorf("unsupported driver bundle schema %d", manifest.SchemaVersion)
	}
	integration, known := integrationByID(manifest.IntegrationID)
	if !known || !integration.BundleInstallSupported {
		return manifest, fmt.Errorf("integration %q does not accept driver bundles", manifest.IntegrationID)
	}
	if manifest.Architecture != currentArchitecture() {
		return manifest, fmt.Errorf("bundle is for %s; this container is %s", manifest.Architecture, currentArchitecture())
	}
	if strings.TrimSpace(manifest.Version) == "" || strings.TrimSpace(manifest.LicenseName) == "" {
		return manifest, fmt.Errorf("bundle version and license_name are required")
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > maxSDRDriverFiles {
		return manifest, fmt.Errorf("manifest contains an invalid number of files")
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return manifest, err
	}
	staging, err := os.MkdirTemp(root, "."+manifest.IntegrationID+"-staging-")
	if err != nil {
		return manifest, err
	}
	defer os.RemoveAll(staging)

	declared := make(map[string]bool, len(manifest.Files))
	for _, item := range manifest.Files {
		name := strings.ReplaceAll(item.Path, "\\", "/")
		if !validDriverBundlePath(name) || declared[name] {
			return manifest, fmt.Errorf("manifest path %q is invalid or duplicated", item.Path)
		}
		zipped := filesByName[name]
		if zipped == nil {
			return manifest, fmt.Errorf("manifest file %q is missing from archive", name)
		}
		declared[name] = true
		source, err := zipped.Open()
		if err != nil {
			return manifest, err
		}
		destination := filepath.Join(staging, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			_ = source.Close()
			return manifest, err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			_ = source.Close()
			return manifest, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(source, int64(zipped.UncompressedSize64)+1))
		closeErr := output.Close()
		_ = source.Close()
		if copyErr != nil {
			return manifest, copyErr
		}
		if closeErr != nil {
			return manifest, closeErr
		}
		actual := hex.EncodeToString(hash.Sum(nil))
		if !strings.EqualFold(actual, strings.TrimSpace(item.SHA256)) {
			return manifest, fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	if len(declared) != len(filesByName) {
		return manifest, fmt.Errorf("archive contains shared libraries not declared in manifest")
	}
	manifest.InstalledAt = time.Now().UTC()
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return manifest, err
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), append(manifestData, '\n'), 0644); err != nil {
		return manifest, err
	}

	target := filepath.Join(root, manifest.IntegrationID)
	if fileExists(filepath.Join(target, "manifest.json")) {
		backup := target + ".backup-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(target, backup); err != nil {
			return manifest, fmt.Errorf("backup existing driver bundle: %w", err)
		}
	}
	if err := os.Rename(staging, target); err != nil {
		return manifest, fmt.Errorf("activate driver bundle: %w", err)
	}
	return manifest, nil
}

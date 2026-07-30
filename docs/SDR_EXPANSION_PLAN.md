# SDR expansion plan

## Baseline findings

UberSDR is a Go web, session, and monitoring application rather than a hardware driver. A single `RadiodController` communicates with KA9Q `radiod` over multicast, and sessions are expressed as radiod channels identified by SSRC. The included receiver template is RX888-specific. The Docker deployment grants USB access only to the KA9Q container, so device support today is defined by that container's KA9Q Radio build and configuration.

The decoder scheduler consumes demodulated PCM/WAV audio from radiod. Its built-in modes are WSPR, FT8, FT4, JS8, and FT2; mode selection, binary paths, CLI arguments, parser expectations, reporting, and lifecycle policy are currently coupled to a `DecoderMode` enum.

This is a strong architecture for one wideband KA9Q receiver, but it assumes a multicast KA9Q control plane, radiod presets, a single source namespace, and a small fixed decoder list.

## Target architecture

Keep the existing session API stable while adding a receiver layer underneath it:

1. **SDR backend**: owns discovery, opening a source, tuning, gain, clock state, and a capability declaration (frequency limits, sample rate, channels, IQ, GPSDO).
2. **source adapter**: normalizes a device or network source to timestamped complex IQ with explicit sample format, rate, center frequency, and discontinuity events.
3. **channel engine**: makes demodulated audio, spectrum tiles, and optional IQ taps from normalized IQ. KA9Q radiod remains the first channel-engine adapter during migration.
4. **monitor scheduler**: allocates tuners/bandwidth using priority, dwell time, CPU budget, and recording policy; it must never assume that every frequency is visible from one capture.
5. **decoder plugins**: declare accepted input, timing/window needs, parser, result schema, and reporting targets. Plugins run as trusted built-ins or out-of-process adapters with a versioned RPC protocol—never arbitrary in-process downloads.

The initial implementation adds a capability-oriented `SDRBackendRegistry`
with native and external-radiod paths, a catalog covering every receive driver
currently exposed by KA9Q Radio, configuration-driven RF coverage, and a
radiod configuration preview generator. It also moves the five built-in
decoder definitions behind a `DecoderPluginRegistry`. Omitting the new receiver
block preserves the existing RX-888/10 kHz–30 MHz behavior.

## Plug-and-play Docker hardware manager

The standard Compose stack now contains a one-shot `sdr-autoconfig` preflight
service. It reads Linux USB sysfs directly, selects a deterministic device
profile, and atomically writes both the KA9Q Radio and UltraSDR receiver
configuration before the radio starts. A clean checkout no longer needs a
sibling `ka9q-radio` source tree: `docker/Dockerfile.radio` builds a pinned
KA9Q Radio revision with the open drivers available from Ubuntu.

If no usable SDR is attached, if several SDRs need an operator choice, or if a
vendor SDK is missing, preflight starts a synthetic setup receiver. This keeps
the Web UI available instead of allowing the stack to crash-loop. Admin →
Radiod → SDR Hardware Setup can rescan, select, apply, and restart the receiver
services. Existing hand-written radiod configurations are preserved unless the
operator explicitly applies a detected profile or sets
`SDR_AUTOCONFIG_FORCE=true`.

The safety defaults are intentional:

- Bias-T and other antenna power outputs are always off.
- Receive-only operation is the only exposed mode, including on transceivers.
- Conservative, driver-supported sample rates are preferred over headline
  maximum rates.
- Multiple matching devices require a serial selection.
- Full tunable coverage and instantaneous visible bandwidth are reported
  separately.
- Unknown USB devices are listed but never guessed from a generic USB class.

### Automatic hardware tiers

| Family | Detection | Default container | First-run result |
| --- | --- | --- | --- |
| RX-888 variants | Cypress VID/PIDs from KA9Q udev rules | Native | 64.8 MSPS wideband HF |
| RTL-SDR and common OEM IDs | librtlsdr udev catalog | Native | 1.8 MSPS, VHF profile, AGC on |
| Airspy HF+ | Official KA9Q VID/PID | Native | 768 kSPS HF profile |
| Airspy R2/Mini | Official KA9Q VID/PID and product metadata | Native | conservative R2 profile |
| HackRF | Official KA9Q VID/PID | Native | 10 MSPS receive-only |
| bladeRF | Nuand/OpenMoko IDs | Native | 10 MSPS receive-only |
| FUNcube Pro/Pro+ | Official KA9Q udev IDs | Native | fixed audio sample rate |
| Fobos / HydraSDR | Official ID or product name | Bundle | detected; requests matching module bundle |
| SDRplay RSP | SDRplay vendor ID | Bundle | detected; requests official API/module bundle |
| LimeSDR, USRP, Pluto, Perseus, ELAD, network SDRs | Adapter discovery (planned) | External bridge | use `external-radiod` until a signed adapter bundle exists |

“All SDRs” cannot safely mean treating arbitrary USB hardware as an SDR.
Support is capability-based: recognized hardware gets tested defaults, an
unknown device stays unknown, and new families are added by a profile plus a
radio/source adapter. This prevents a coincidental VID/PID or unsafe gain/bias
setting from damaging hardware.

### Proprietary driver installation

The Hardware Setup panel accepts an UltraSDR driver-bundle ZIP after explicit
vendor-license acceptance. It does not receive a package manager, shell, or
Docker socket. Bundles are declarative and architecture-specific:

```json
{
  "schema_version": 1,
  "integration_id": "sdrplay",
  "version": "3.x",
  "architecture": "linux/amd64",
  "license_name": "Vendor API license",
  "files": [
    {"path": "modules/sdrplay.so", "sha256": "<hex>"},
    {"path": "lib/libsdrplay_api.so.3", "sha256": "<hex>"}
  ]
}
```

Only `modules/*.so*` and `lib/*.so*` are accepted; every file must be declared
and checksum-matched, traversal paths and extra files are rejected, and the
previous bundle is backed up before activation. This supports one-click
installation of artifacts that the vendor permits the operator to download,
without silently accepting a license or redistributing restricted binaries.

### Host USB reality

- Native Linux Docker: `/dev/bus/usb` and USB sysfs are mounted by the supplied
  Compose file; plug the receiver in before startup or rescan in Hardware Setup.
- Windows Docker Desktop/WSL2: Windows must first attach the USB device to the
  Linux VM with a supported USB/IP workflow. A container cannot claim a USB
  device that Windows still owns.
- macOS Docker Desktop: direct generic USB passthrough is not provided; run the
  hardware radio/Soapy bridge on a USB-capable Linux host and select
  `external-radiod`.

These are host hypervisor constraints, not settings a browser inside the
container can bypass.

## Priority hardware families

| Priority | Families | Why / first path |
| --- | --- | --- |
| P0 | KA9Q Radio with RX888/USRP-class wideband frontends | Preserve and document the production path; supports wide HF monitoring now. |
| P1 | RTL-SDR v3/v4 and compatible rtl_tcp | Lowest-cost receive-only deployment; use a supervised `rtl_tcp`/Soapy adapter first for portability. |
| P1 | Airspy HF+ and Airspy R2/Mini | Strong HF/VHF performance; implement through SoapySDR or native sidecar where its controls matter. |
| P1 | SDRplay RSP family | Broad coverage and popular hardware; sidecar isolates vendor API/licensing and USB permissions. |
| P2 | HackRF One, LimeSDR, bladeRF | Wideband experiments and transmit-capable hardware; receive-only initially, with explicit TX interlocks later. |
| P2 | Ettus USRP, ADALM-Pluto, Red Pitaya | Network/high-rate and synchronized deployments; use UHD/IIO sidecars and expose clock/PTP/GPSDO state. |
| P2 | KiwiSDR / OpenWebRX / SpyServer / rtl_tcp remote sources | Enables geographically distributed monitoring without local USB access. |

Use SoapySDR where it provides a reliable common denominator, but retain backend-specific sidecars for timing, bias tee, preselectors, coherent channels, and device-specific calibration.

## Monitoring and decoding roadmap

### Phase 1 — HF and existing audio pipeline

- Migrate WSPR, FT8, FT4, JS8, and FT2 to registered plugins (started).
- Add AM broadcast/utility monitoring, CW/RBN, NAVTEX, weather fax, DRM, FreeDV, and digital audio extension results to a common event schema.
- Add receiver capability checks to band/decoder validation and operator-visible coverage gaps.

### Phase 2 — VHF/UHF spectrum monitoring

- Add narrowband FM/AM/SSB monitoring and signal classification.
- Add ADS-B (1090 MHz), ACARS/VDL2, AIS (161.975/162.025 MHz), APRS (144.39 MHz regional), NOAA APT, DMR, P25, NXDN, dPMR, TETRA, and pager workflows where lawful.
- Keep trunked/digital-voice handling as opt-in plugins with local legal and access controls; do not build interception or decryption features.

### Phase 3 — microwave/satellite and distributed sensing

- Add 433/868/915 MHz ISM telemetry, LoRaWAN gateways, GOES/HRPT, Inmarsat AERO, satellite beacon tracking, and direction/TDOA workflows where hardware timing supports them.
- Add multi-receiver scheduling, health scoring, calibrated occupancy maps, recordings with provenance, and an event bus for alerts.

## Delivery sequence

1. Add a `receiver.backend` configuration block and select `ka9q-radiod` by default. **Implemented.**
2. Catalog all current KA9Q receive drivers and add a generic externally managed radiod/Soapy path. **Implemented.**
3. Introduce a versioned sidecar protocol for direct-IQ sources; ship `rtl_tcp` and SoapySDR reference adapters.
4. Adapt the channel engine so radiod and direct-IQ adapters both satisfy the existing session/spectrum contracts.
5. Add plugin manifests, an out-of-process decoder runner, resource limits, and normalized events.
6. Add monitoring schedules, storage quotas, receiver health/clock metrics, and UI coverage reporting.

## Compatibility and safety rules

- Preserve the current radiod multicast + SSRC behavior until the direct-IQ path reaches feature parity.
- Treat frequency, gain, bandwidth, sample format, and timestamps as explicit metadata; do not infer them from a decoder mode.
- Default new device backends to receive-only. Any future transmit support requires separate credentials, physical/firmware interlocks, band-plan enforcement, and explicit operator enablement.
- Enforce per-plugin CPU, memory, runtime, output-size, and network permissions. Record only operator-authorized bands and retention periods.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/doctor"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/reconciler"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/unit"
)

// doctorConfig is what the checks need, from the settings the agent runs with, so that
// the doctor judges the node by its own configuration.
func doctorConfig(cfg config, cp *cpclient.Client) doctor.Config {
	return doctor.Config{
		NodeID:            cfg.NodeID,
		DryRun:            cfg.DryRun,
		GuestKernel:       cfg.GuestKernel,
		GuestRootFS:       cfg.GuestRootFS,
		GuestVerify:       strings.ToLower(strings.TrimSpace(cfg.GuestVerify)),
		DiskDir:           cfg.DiskDir,
		DiskMinFreeMiB:    diskMinFreeMiB(cfg.DiskMinFreeMiB, cfg.GuestRootFS),
		CHBinary:          cfg.VMMBinary,
		VirtiofsdBin:      cfg.VirtiofsdBin,
		Confine:           strings.ToLower(strings.TrimSpace(cfg.VMConfine)),
		TapAuto:           cfg.TapAuto,
		HostVsock:         cfg.HostVsock,
		HostVsockDir:      cfg.HostVsockDir,
		EgressRedirect:    cfg.EgressNFTRedirect,
		EgressProxyListen: cfg.EgressProxyListen,
		NFTMode:           cfg.NFTEgressMode,
		ControlPlane: func(ctx context.Context) error {
			if cp == nil {
				return fmt.Errorf("no control-plane client")
			}
			// A poll of the node's own work: it needs the node's credential and a node the
			// control plane knows, and changes nothing.
			_, err := cp.ListWork(ctx, cfg.NodeID)
			return err
		},
	}
}

// doctorChecks are the standard checks plus the ones that need what only this binary
// knows: whether the VMMs can run as users of their own.
func doctorChecks(cfg config, cp *cpclient.Client) []doctor.Check {
	h := doctor.RealHost(reconciler.DiskFreeMiB, fileSHA256)
	dc := doctorConfig(cfg, cp)
	checks := doctor.Standard(h, dc)
	vmUser := doctor.Check{Name: "vmm-user", Run: func(ctx context.Context) doctor.Result {
		return checkVMMUser(cfg, unit.Available(), hostVMUserProbes())
	}}
	// Before the control plane, which is the last line a reader looks at.
	return append(checks[:len(checks)-1:len(checks)-1], vmUser, checks[len(checks)-1])
}

// checkVMMUser says whether each Cloud Hypervisor can run as a user of its own on
// this host (--vm-unprivileged), by the probes the agent itself runs at start.
func checkVMMUser(cfg config, available error, probes vmUserProbes) doctor.Result {
	mode := strings.ToLower(strings.TrimSpace(cfg.VMUnprivileged))
	if cfg.DryRun || mode == "off" {
		return doctor.Result{Status: doctor.Skip, Detail: "the VMMs run as root (--vm-unprivileged=off or dry-run)"}
	}
	confine, err := vmConfinement(cfg, available)
	if err != nil {
		return doctor.Result{Status: doctor.Fail, Detail: err.Error()}
	}
	if confine == nil {
		if mode == "on" {
			return doctor.Result{Status: doctor.Fail, Detail: "--vm-unprivileged=on needs each VM in a unit of its own (--vm-confine), and this host cannot: " + fmt.Sprint(available),
				Fix: "run on a host with systemd as root, or set --vm-unprivileged=auto"}
		}
		return doctor.Result{Status: doctor.Skip, Detail: "VMs are not confined in units of their own, so they run as root"}
	}
	probe := cfg
	probe.VMUnprivileged = "on" // ask what "on" would say, whatever the mode is
	u, err := vmUnprivileged(probe, confine, probes)
	if err != nil {
		status := doctor.Warn
		if mode == "on" {
			status = doctor.Fail
		}
		return doctor.Result{Status: status, Detail: err.Error(), Fix: "see docs/concepts/node-runtime.md (the unprivileged VMM); --vm-unprivileged=off keeps the VMMs root"}
	}
	return doctor.Result{Status: doctor.OK, Detail: fmt.Sprintf("each Cloud Hypervisor can run as its own user (%d + the VM's CID)", u.UIDBase)}
}

// fileSHA256 is the digest of a file, "sha256:<hex>".
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// runDoctor is --doctor: the checks of the node with this configuration, printed, and
// an exit code that is 1 when one failed. It builds the control-plane client the way
// the agent does, from the certificates in --cert-dir or the API key.
func runDoctor(cfg config, asJSON bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cp, err := doctorControlPlane(cfg)
	if err != nil {
		slog.Warn("the control plane is not checked", "reason", err)
	}
	rep := doctor.Run(ctx, cfg.NodeID, doctorChecks(cfg, cp), 20*time.Second)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		fmt.Print(rep.Text())
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

// doctorControlPlane is the client the agent would use to talk to the control plane.
func doctorControlPlane(cfg config) (*cpclient.Client, error) {
	httpClient, _, err := cpclient.LoadMTLSClientCert(cfg.CertDir, cfg.MTLS, cfg.ControlPlaneCA)
	if err != nil {
		return nil, err
	}
	cp := cpclient.New(cfg.ControlPlaneURL, httpClient)
	key, err := loadNodeAPIKey(cfg)
	if err != nil {
		return nil, err
	}
	cp.APIKey = key
	return cp, nil
}

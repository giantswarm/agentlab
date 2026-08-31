package lab

// `agentlab trust` / `agentlab untrust`: installing the lab CA into the OS
// and browser trust stores, and probing whether it is there. The heavy
// lifting is smallstep/truststore — the mechanism mkcert uses — which
// shells out to the platform's own tooling (update-ca-certificates /
// update-ca-trust on Linux, the security tool on macOS, certutil on
// Windows) and wraps the privileged steps in sudo itself. Trust changes are
// explicit by design: `up` and `down` never touch a trust store.

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/smallstep/truststore"

	"github.com/giantswarm/agentplatform-kind/internal/config"
)

// Trust installs the lab CA into the system trust store (one sudo prompt)
// and, when NSS tooling is available, into the Firefox/Chromium NSS
// profiles. Reversible: `agentlab untrust` removes exactly what this
// installed.
func Trust(cfg *config.Config) error {
	// Mint or re-mint first (a domain change re-mints the CA), so what gets
	// installed is exactly the CA the lab serves with.
	if err := GenCerts(cfg.Platform.Domain, false); err != nil {
		return err
	}
	if err := sweepReplacedCAs(); err != nil {
		return err
	}
	cert := readCertFile(caCertPath)
	if cert == nil {
		return fmt.Errorf("%s is missing or unreadable", caCertPath)
	}

	if certTrustedBySystem(cert) {
		note("system store: already trusted")
	} else {
		fmt.Println("Installing the lab CA into the system trust store (sudo may prompt)")
		if err := truststore.InstallFile(caCertPath); err != nil {
			return trustErr("installing into the system store", err)
		}
		note("system store: installed")
	}
	trustNSS(cert)

	fmt.Println()
	fmt.Printf("Browsers get a green lock on https://*.%s and the Dex login now\n", cfg.Platform.Domain)
	fmt.Println("(restart any running browser once). " + nodeTrustHint())
	fmt.Println("Reversible any time: agentlab untrust")
	return nil
}

// Untrust removes the lab CA — and any predecessor a re-mint stashed —
// from the system store and the NSS profiles. certs/ stays on disk, so a
// later `agentlab trust` reinstalls the same CA. (`agentlab down` never
// touches the trust stores; this is the only removal path.)
func Untrust(cfg *config.Config) error {
	if err := sweepReplacedCAs(); err != nil {
		return err
	}
	cert := readCertFile(caCertPath)
	if cert == nil {
		fmt.Printf("No lab CA at %s — nothing to untrust.\n", caCertPath)
		return nil
	}
	if err := untrustCert(caCertPath, cert); err != nil {
		return err
	}
	fmt.Println("certs/ kept on disk; `agentlab trust` re-installs the same CA.")
	return nil
}

// SystemTrusted reports whether the lab CA on disk is in the system trust
// store — the probe behind the `up` hints and the TUI trust row. Never
// installs anything.
func SystemTrusted() bool {
	cert := readCertFile(caCertPath)
	return cert != nil && certTrustedBySystem(cert)
}

// certTrustedBySystem verifies cert against the OS trust store. Go
// snapshots the system pool once per process, so the answer is
// point-in-time: fresh for every CLI run; the TUI's row catches up on its
// next start.
func certTrustedBySystem(cert *x509.Certificate) bool {
	_, err := cert.Verify(x509.VerifyOptions{})
	return err == nil
}

// trustNSS covers Firefox and Chromium-on-Linux, which read their own NSS
// databases instead of the system store. Skips with a hint when the NSS
// certutil tool is missing — the same caveat mkcert has. (Firefox can also
// be pointed at the system store: security.enterprise_roots.enabled.)
func trustNSS(cert *x509.Certificate) {
	nss, err := truststore.NewNSSTrust()
	if err != nil {
		note("firefox/chromium NSS: skipped — no certutil; install it (%s) and re-run `agentlab trust`", certutilHint())
		return
	}
	if err := nss.PreCheck(); err != nil {
		note("firefox/chromium NSS: no profile databases found, nothing to do")
		return
	}
	if nss.Exists(cert) {
		note("firefox/chromium NSS: already trusted")
		return
	}
	if err := nss.Install(caCertPath, cert); err != nil {
		note("firefox/chromium NSS: failed (%v)", err)
		return
	}
	note("firefox/chromium NSS: installed")
}

// untrustCert removes one CA from the NSS profiles and the system store,
// touching only stores that actually hold it (so no sudo prompt for a CA
// that was never trusted).
func untrustCert(path string, cert *x509.Certificate) error {
	if nss, err := truststore.NewNSSTrust(); err == nil && nss.PreCheck() == nil && nss.Exists(cert) {
		if err := nss.Uninstall(path, cert); err != nil {
			return fmt.Errorf("removing from NSS: %w", err)
		}
		note("firefox/chromium NSS: removed")
	}
	if !certTrustedBySystem(cert) {
		note("system store: not installed, nothing to do")
		return nil
	}
	fmt.Println("Removing the lab CA from the system trust store (sudo may prompt)")
	if err := truststore.UninstallFile(path); err != nil {
		return trustErr("removing from the system store", err)
	}
	note("system store: removed")
	return nil
}

// sweepReplacedCAs clears CAs an earlier re-mint replaced (stashed under
// certs/replaced/) out of the trust stores, then deletes the stash. Both
// trust and untrust run it, so a replaced-but-still-trusted root never
// survives the next trust operation.
func sweepReplacedCAs() error {
	stashed, err := filepath.Glob(filepath.Join(replacedCADir, "*.crt"))
	if err != nil {
		return err
	}
	for _, path := range stashed {
		if cert := readCertFile(path); cert != nil {
			fmt.Printf("Cleaning up the replaced lab CA (serial %s)\n", cert.SerialNumber)
			if err := untrustCert(path, cert); err != nil {
				return err
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// trustErr keeps the failing command's output visible: truststore's own
// error says only which binary failed, and the reason (a sudo refusal, a
// keychain denial) is in the swallowed output.
func trustErr(action string, err error) error {
	var cmdErr *truststore.CmdError
	if errors.As(err, &cmdErr) {
		if out := strings.TrimSpace(string(cmdErr.Out())); out != "" {
			return fmt.Errorf("%s: %w: %s", action, err, out)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

// nodeTrustHint is the Node.js line for the detected Node:
// NODE_USE_SYSTEM_CA replaces the per-shell NODE_EXTRA_CA_CERTS export on
// Node >= 22.15.
func nodeTrustHint() string {
	if nodeSupportsSystemCA() {
		return "Node picks it up with NODE_USE_SYSTEM_CA=1 (no cert exports)."
	}
	return "Node >= 22.15 picks it up with NODE_USE_SYSTEM_CA=1; older Node still needs NODE_EXTRA_CA_CERTS=" + absCAPath() + "."
}

// nodeSupportsSystemCA reports whether the host's node (if any) understands
// NODE_USE_SYSTEM_CA=1, which landed in Node 22.15.
func nodeSupportsSystemCA() bool {
	out, err := outputQuiet("node", "--version")
	if err != nil {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(out), "v"), ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > 22 || (major == 22 && minor >= 15)
}

// certutilHint names the package that ships NSS's certutil on this OS.
func certutilHint() string {
	if runtime.GOOS == "darwin" {
		return "brew install nss"
	}
	return "apt install libnss3-tools / dnf install nss-tools / pacman -S nss"
}

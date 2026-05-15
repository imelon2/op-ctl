package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	sshconfig "github.com/kevinburke/ssh_config"
	skeemaknownhosts "github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"
)

// BastionConfig is the minimal description of a bastion the dialer
// understands. It is decoupled from internal/config.Bastion so the
// sshtunnel package does not pull TOML/decoder dependencies and can be
// reused with non-TOML sources (e.g. a future flag-based override).
//
// Field semantics mirror internal/config.Bastion verbatim; main.go is
// the conversion seam.
//
// ProxyJump, when non-empty, names another bastion alias the resolver
// must dial FIRST. The resolver chains BastionConfigs by recursively
// resolving each level's ProxyJump until it hits a level with no parent.
type BastionConfig struct {
	Alias             string
	Host              string
	Port              int
	User              string
	IdentityFile      string
	KnownHosts        string
	ProxyJump         string
	KeepaliveInterval time.Duration
}

// netDialer is the surface dialSSHThrough uses to open the TCP connection
// to the bastion. *net.Dialer satisfies it for the first hop; *ssh.Client
// satisfies it for every subsequent hop in a ProxyJump chain so the chain
// builder doesn't have to special-case the boundary.
type netDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

var _ netDialer = (*net.Dialer)(nil)
var _ netDialer = (*ssh.Client)(nil)

// SSHConfigLookup is the read-side interface against ~/.ssh/config the
// resolver uses to fill in fields that the inline BastionConfig omits.
// Returning "" for an unknown alias/key is the canonical "not present"
// signal (matches kevinburke/ssh_config.Get).
type SSHConfigLookup interface {
	Get(alias, key string) string
}

// DefaultSSHConfigLookup is the production lookup backed by
// kevinburke/ssh_config (which reads ~/.ssh/config + /etc/ssh/ssh_config
// on first call and caches the parse tree).
type DefaultSSHConfigLookup struct{}

func (DefaultSSHConfigLookup) Get(alias, key string) string {
	return sshconfig.Get(alias, key)
}

// dialSSH opens a brand-new *ssh.Client to the bastion described by cfg
// using a fresh *net.Dialer for the TCP leg. This is the single-hop
// entry point used when a bastion has no proxy_jump parent.
//
// For chained dials (ProxyJump), the resolver calls dialSSHThrough
// directly with the parent *ssh.Client as the underlying dialer.
func dialSSH(ctx context.Context, cfg BastionConfig, lookup SSHConfigLookup) (*ssh.Client, error) {
	return dialSSHThrough(ctx, &net.Dialer{}, cfg, lookup)
}

// dialSSHThrough opens a brand-new *ssh.Client to the bastion described
// by cfg, using `underlying` for the TCP leg. The underlying dialer is
// either a *net.Dialer (first hop from the laptop) or a *ssh.Client
// (any subsequent hop reachable only through a previous SSH tunnel).
//
// ctx.Deadline (if any) bounds the entire TCP+SSH handshake leg via the
// underlying dialer's DialContext + ssh.NewClientConn — plain ssh.Dial
// does NOT honor ctx and would silently ignore caller-supplied
// deadlines.
//
// Host-key verification uses knownhosts.New against the resolved
// known_hosts file. The dialer never accepts unknown host keys: the
// package contract is secure-by-default, and a missing or unknown host
// key surfaces with an actionable, ssh-keyscan-style error.
func dialSSHThrough(ctx context.Context, underlying netDialer, cfg BastionConfig, lookup SSHConfigLookup) (*ssh.Client, error) {
	if cfg.Host == "" {
		// inline didn't set Host; fall back to ssh_config HostName
		cfg.Host = lookup.Get(cfg.Alias, "HostName")
		if cfg.Host == "" {
			return nil, fmt.Errorf("ssh bastion %q: no host (neither inline nor ~/.ssh/config)", cfg.Alias)
		}
	}
	if cfg.User == "" {
		cfg.User = lookup.Get(cfg.Alias, "User")
		if cfg.User == "" {
			cfg.User = os.Getenv("USER")
		}
	}
	if cfg.Port == 0 {
		if p := lookup.Get(cfg.Alias, "Port"); p != "" {
			fmt.Sscanf(p, "%d", &cfg.Port)
		}
		if cfg.Port == 0 {
			cfg.Port = 22
		}
	}
	knownHosts := cfg.KnownHosts
	if knownHosts == "" {
		knownHosts = lookup.Get(cfg.Alias, "UserKnownHostsFile")
	}
	if knownHosts == "" {
		knownHosts = "~/.ssh/known_hosts"
	}
	knownHostsExpanded, err := expandPath(knownHosts)
	if err != nil {
		return nil, fmt.Errorf("ssh bastion %q known_hosts: %w", cfg.Alias, err)
	}
	if _, err := os.Stat(knownHostsExpanded); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"known_hosts file %s not found — create with: ssh-keyscan -H <bastion-host> > %s",
				knownHostsExpanded, knownHostsExpanded,
			)
		}
		return nil, fmt.Errorf("ssh bastion %q known_hosts %s: %w", cfg.Alias, knownHostsExpanded, err)
	}
	db, err := skeemaknownhosts.NewDB(knownHostsExpanded)
	if err != nil {
		return nil, fmt.Errorf("ssh bastion %q known_hosts %s: %w", cfg.Alias, knownHostsExpanded, err)
	}

	auths, err := buildAuthMethods(cfg, lookup)
	if err != nil {
		return nil, fmt.Errorf("ssh bastion %q auth: %w", cfg.Alias, err)
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("ssh bastion %q: no auth methods available (no SSH_AUTH_SOCK, no identity_file)", cfg.Alias)
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	// Pin negotiation to the host-key algorithms we actually have entries
	// for. Without this, Go's default order (ECDSA → RSA → ED25519) wins —
	// a host recorded only as ed25519 fails with "key mismatch" because
	// the server signs with ECDSA and known_hosts has no ECDSA entry.
	// Mirrors OpenSSH client behavior. Empty list (host not in
	// known_hosts) falls through to Go defaults; the callback then
	// reports the host as unknown with our actionable error.
	hostKeyAlgos := db.HostKeyAlgorithms(addr)

	clientCfg := &ssh.ClientConfig{
		User:              cfg.User,
		Auth:              auths,
		HostKeyAlgorithms: hostKeyAlgos,
		HostKeyCallback:   wrapHostKeyCallback(db.HostKeyCallback(), cfg.Host, cfg.Port, knownHostsExpanded),
	}

	conn, err := underlying.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh bastion %q dial %s: %w", cfg.Alias, addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh bastion %q handshake %s: %w", cfg.Alias, addr, err)
	}
	// Clear any deadline so the long-lived SSH session is not bounded
	// by the dial-time context.
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// wrapHostKeyCallback turns a knownhosts rejection into an actionable
// error. Two distinct failure modes are surfaced with separate remediation
// hints because they need different operator action:
//
//   - "host not in known_hosts" → run ssh-keyscan to ADD the entry.
//   - "host key mismatch" → entry exists but server's key differs.
//     Run ssh-keygen -R first, THEN ssh-keyscan, otherwise the new entry
//     joins a stale one and OpenSSH-compatible clients keep failing.
//
// Both branches are produced by x/crypto's *knownhosts.KeyError: an empty
// Want list means the host is unknown, a non-empty Want list means the
// recorded keys don't match what the server presented.
func wrapHostKeyCallback(inner ssh.HostKeyCallback, host string, port int, knownHostsPath string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if err := inner(hostname, remote, key); err != nil {
			var keyErr *xknownhosts.KeyError
			if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
				return fmt.Errorf(
					"host key for %s:%d MISMATCHES recorded entry in %s — server's key differs from what's stored. "+
						"Run: ssh-keygen -R %s && ssh-keyscan -H %s >> %s (underlying: %w)",
					host, port, knownHostsPath, host, host, knownHostsPath, err,
				)
			}
			return fmt.Errorf(
				"host key for %s:%d not in %s — run: ssh-keyscan -H %s >> %s (underlying: %w)",
				host, port, knownHostsPath, host, knownHostsPath, err,
			)
		}
		return nil
	}
}

// buildAuthMethods returns the auth-method chain in precedence order:
//  1. SSH agent ($SSH_AUTH_SOCK), if reachable
//  2. inline IdentityFile, if set
//  3. ssh_config IdentityFile for this alias, if set
//
// Each present method is appended in order — the SSH server picks the
// first that succeeds. An empty result means the caller has neither an
// agent nor any key path: dialSSH treats that as a fatal "no auth".
func buildAuthMethods(cfg BastionConfig, lookup SSHConfigLookup) ([]ssh.AuthMethod, error) {
	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if c, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(c).Signers))
		}
	}
	identityFile := cfg.IdentityFile
	if identityFile == "" {
		identityFile = lookup.Get(cfg.Alias, "IdentityFile")
	}
	if identityFile != "" {
		expanded, err := expandPath(identityFile)
		if err != nil {
			return nil, err
		}
		key, err := os.ReadFile(expanded)
		if err != nil {
			// Missing identity file is not fatal IF the agent provided
			// something — the SSH server will tell us which keys work.
			if len(auths) == 0 {
				return nil, fmt.Errorf("read identity_file %s: %w", expanded, err)
			}
		} else {
			signer, err := ssh.ParsePrivateKey(key)
			if err != nil {
				return nil, fmt.Errorf("parse identity_file %s: %w", expanded, err)
			}
			auths = append(auths, ssh.PublicKeys(signer))
		}
	}
	return auths, nil
}

// expandPath supports bare "~" / "~/..." and $VAR/${VAR} substitution.
// The "~user" form is rejected so the dialer matches what
// internal/config.validatePathForm allowed at decode time.
func expandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "~") {
		if p != "~" && !strings.HasPrefix(p, "~/") {
			return "", fmt.Errorf("path %q: ~user form is not supported", p)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			p = home
		} else {
			p = filepath.Join(home, p[2:])
		}
	}
	return os.ExpandEnv(p), nil
}

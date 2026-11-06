// TetherFlow Steam accelerator is separate from the legacy PAC companion.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tetherflow-desktop/internal/steamaccel"
	"tetherflow-desktop/internal/steamsetup"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	home := flag.String("home-ip", "", "IPv4 assigned to the home Wi-Fi interface (required)")
	phone := flag.String("phone-proxy", "127.0.0.1:8283", "existing local tunnel endpoint; requires --external-tunnel")
	externalTunnel := flag.Bool("external-tunnel", false, "use existing iproxy/other loopback tunnel instead of built-in Apple USB client")
	udid := flag.String("device-udid", "", "optional USB device serial/UDID selection when multiple phones are attached")
	phones := flag.Bool("list-phones", false, "list USB-only Apple devices without modifying settings")
	httpOnly := flag.Bool("http-only", false, "disable opaque HTTPS/SNI port443 passthrough; HTTP80 only")
	enable := flag.Bool("enable-steam", false, "explicitly install temporary Steam discovery hosts entry (Windows administrator)")
	benchmark := flag.Bool("check-phone", false, "verify 128 MiB USB application-path payload and print Mbps; not a cellular test")
	doctor := flag.Bool("doctor", false, "print local IPv4 interfaces and prerequisites without changing settings")
	report := flag.String("report", "tetherflow-status.json", "local JSON diagnostic summary (no token or download URLs)")
	homeWeight := flag.Int("home-weight", 1, "relative home network capacity")
	phoneWeight := flag.Int("phone-weight", 4, "relative phone network capacity")
	extraHosts := flag.String("allow-hosts", "", "comma-separated explicit CDN suffixes; replaces default allowlist")
	guardian := flag.Bool("guardian", false, "internal recovery process")
	parent := flag.Int("parent", 0, "internal parent process")
	parentHandle := flag.Uint64("parent-handle", 0, "internal inherited parent handle")
	lease := flag.String("lease", "", "internal recovery lease")
	recoveryLog := flag.String("recovery-log", "", "file that receives hosts-cleanup failures with the lease (default: next to --report)")
	recoverLease := flag.String("recover-lease", "", "remove only a matching stale session entry; does not start relay")
	flag.Parse()
	if *recoveryLog == "" {
		*recoveryLog = defaultRecoveryLog(*report)
	}
	if *guardian {
		return guard(*parent, *lease, *parentHandle, *recoveryLog)
	}
	if *doctor {
		return diagnose()
	}
	if *phones {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return listPhones(ctx)
	}
	if *recoverLease != "" {
		p, e := hostsPath()
		if e != nil {
			return e
		}
		return cleanupHosts(context.Background(), p, *recoverLease, mainCleanupBudget, flushDNS, "")
	}
	if !*benchmark {
		if err := validateHome(*home); err != nil {
			return err
		}
	}
	if !*externalTunnel {
		scanCtx, scanCancel := context.WithTimeout(context.Background(), 8*time.Second)
		endpoint, cleanupTunnel, tunnelErr := startPhoneTunnel(scanCtx, *udid)
		scanCancel()
		if tunnelErr != nil {
			return tunnelErr
		}
		defer cleanupTunnel()
		*phone = endpoint
	}
	// The session token is typed into this process's own hidden stdin prompt; it is probed
	// with the authenticated diagnostic before anything else is changed.
	tokens := newTokenSource(os.Stdin, os.Stderr)
	defer tokens.Close()
	token, err := acquireStartupToken(tokens.read, func(candidate string) error {
		return probePhone(context.Background(), *phone, candidate)
	}, log.Printf)
	if err != nil {
		return err
	}
	if *benchmark {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 30*time.Second)
		rate, checkErr := checkPhone(checkCtx, *phone, token, 128*1024*1024)
		checkCancel()
		if checkErr != nil {
			return fmt.Errorf("iPhone USB application-path preflight failed; no settings changed: %w", checkErr)
		}
		fmt.Printf("Verified USB app-path payload: %.1f Mbps (not cellular or Steam speed)\n", rate)
		return nil
	}
	tokens.remember(token)
	log.Print("Authenticated iPhone USB diagnostic passed (not a cellular speed test).")
	cfg := steamaccel.Config{HomeIPv4: *home, AllowHomePeer: true, PhoneProxyAddr: *phone, PhoneToken: token, HomeWeight: *homeWeight, PhoneWeight: *phoneWeight}
	if *extraHosts != "" {
		cfg.AllowedHosts = strings.Split(*extraHosts, ",")
	}
	relay, err := steamaccel.New(cfg)
	if err != nil {
		return err
	}
	defer relay.Close()
	addr := net.JoinHostPort(*home, "80")
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("cannot bind %s; port80 may be in use (no settings changed): %w", addr, err)
	}
	defer listener.Close()
	server := relay.Server(addr)
	var tlsListener net.Listener
	if !*httpOnly {
		tlsAddr := net.JoinHostPort(*home, "443")
		tlsListener, err = net.Listen("tcp4", tlsAddr)
		if err != nil {
			return fmt.Errorf("cannot bind %s for opaque TLS; no settings changed (use --http-only only if intended): %w", tlsAddr, err)
		}
		defer tlsListener.Close()
	}
	var cleanup = func() {}
	if *enable {
		p, e := hostsPath()
		if e != nil {
			return e
		}
		var b [16]byte
		if _, e = rand.Read(b[:]); e != nil {
			return e
		}
		session := hex.EncodeToString(b[:])
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		command := exec.Command(exe, "--guardian", "--parent", strconv.Itoa(os.Getpid()), "--lease", session, "--recovery-log", *recoveryLog)
		releaseHandle, prepErr := prepareGuardian(command)
		if prepErr != nil {
			return fmt.Errorf("recovery guardian setup; no hosts changes: %w", prepErr)
		}
		e = command.Start()
		releaseHandle()
		if e != nil {
			return fmt.Errorf("recovery guardian could not start; no hosts changes: %w", e)
		}
		go func() {
			if e := command.Wait(); e != nil {
				log.Printf("WARNING: recovery guardian exited: %v; graceful cleanup still active", e)
			}
		}()
		// Both ports are held, so a stale entry for this address belongs to a dead session.
		if e = reclaimStale(p, *home, listener); e != nil {
			return fmt.Errorf("Steam discovery not enabled: %w", e)
		}
		if _, e = steamsetup.Update(p, *home, session); e != nil {
			return fmt.Errorf("Steam discovery not enabled: %w", e)
		}
		cleanup = func() {
			// Past this budget the guardian takes over once this process exits.
			ctx, cancel := context.WithTimeout(context.Background(), mainCleanupBudget)
			defer cancel()
			if e := cleanupHosts(ctx, p, session, mainCleanupBudget, flushDNS, *recoveryLog, tokens.secrets()...); e != nil {
				log.Printf("RECOVERY REQUIRED: %v; run --recover-lease %s", e, session)
			}
		}
		defer cleanup()
		if e = flushDNS(); e != nil {
			return fmt.Errorf("DNS flush failed; discovery entry will be removed: %w", e)
		}
		log.Printf("Temporary Steam discovery enabled; recovery lease=%s. Restart Steam once. No system proxy/PAC settings changed.", session)
	}
	if !*enable {
		log.Print("PREVIEW ONLY: Steam discovery NOT installed. Use --enable-steam after reviewing setup.")
	}
	log.Printf("Listening %s. Experimental Steam HTTP / opaque TLS relay; real speed and current Steam adoption UNVERIFIED.", addr)
	log.Print("Keep iOS app foreground and USB connected. Only local PC peers allowed; no TLS decryption. No requests means Steam is NOT using this relay.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Uplinks != steamaccel.UplinkHomeOnly {
		// The only reader of stdin from here on. On shutdown tokens.Close (deferred above)
		// restores console echo even if this goroutine is still blocked in a read.
		go (&refresher{
			status: func() steamaccel.PhoneAuthStatus { return relay.Snapshot().PhoneAuth },
			read:   tokens.read,
			probe: func(ctx context.Context, candidate string) error {
				return probePhone(ctx, *phone, candidate)
			},
			set:      relay.SetPhoneToken,
			remember: tokens.remember,
			logf:     log.Printf,
		}).run(ctx)
	}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.Serve(listener) }()
	if tlsListener != nil {
		go func() { errorsCh <- relay.ServeTLS(tlsListener) }()
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	started := time.Now()
	lastReport := started
	var lastHomeBytes, lastPhoneBytes uint64
	warnedInactive := false
	writeStatus := func() {
		status := relay.Snapshot()
		now := time.Now()
		homeMbps := intervalMbps(lastHomeBytes, status.Home.Bytes, now.Sub(lastReport))
		phoneMbps := intervalMbps(lastPhoneBytes, status.Phone.Bytes, now.Sub(lastReport))
		lastReport, lastHomeBytes, lastPhoneBytes = now, status.Home.Bytes, status.Phone.Bytes
		if !warnedInactive && now.Sub(started) > 45*time.Second && status.Home.Requests+status.Phone.Requests == 0 {
			warnedInactive = true
			log.Print("NOT ACCELERATING: no content requests observed after45s. Confirm Steam cache adoption in content_log.txt; configuration alone is not success.")
		}
		summary := struct {
			Started           time.Time `json:"started"`
			Experimental      bool      `json:"experimental"`
			SteamDiscovery    bool      `json:"steam_discovery_configured"`
			Status            any       `json:"uplinks"`
			HomeMbps          float64   `json:"home_interval_mbps"`
			PhoneMbps         float64   `json:"phone_interval_mbps"`
			BothPathsObserved bool      `json:"both_paths_observed"`
		}{started, true, *enable, status, homeMbps, phoneMbps, status.Home.Bytes > 0 && status.Phone.Bytes > 0}
		if *report != "" {
			encoded, e := json.MarshalIndent(summary, "", "  ")
			if e == nil {
				if e = os.WriteFile(*report, append(encoded, '\n'), 0600); e != nil {
					log.Printf("status report: %v", e)
				}
			}
		}
		log.Printf("Home %.1f Mbps (%d bytes/%d attempts), phone %.1f Mbps (%d bytes/%d attempts), combined %.1f Mbps. Interval accounting, not a verified speed gain.", homeMbps, status.Home.Bytes, status.Home.Requests, phoneMbps, status.Phone.Bytes, status.Phone.Requests, homeMbps+phoneMbps)
	}
	defer writeStatus()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e := server.Shutdown(shutdownCtx); e != nil {
				_ = server.Close()
			}
			return nil
		case e := <-errorsCh:
			if errors.Is(e, http.ErrServerClosed) {
				return nil
			}
			return e
		case <-ticker.C:
			writeStatus()
		}
	}
}

func validateHome(value string) error {
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
		return errors.New("--home-ip must be the RFC1918 IPv4 assigned to your home Wi-Fi; run --doctor")
	}
	ifaces, e := net.Interfaces()
	if e != nil {
		return e
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, e := iface.Addrs()
		if e != nil {
			continue
		}
		for _, a := range addrs {
			assigned, _, e := net.ParseCIDR(a.String())
			if e == nil && assigned.Equal(ip) {
				return nil
			}
		}
	}
	return errors.New("--home-ip is not assigned to an active local interface")
}
func diagnose() error {
	fmt.Println("TetherFlow Steam diagnostic — no network settings changed")
	ifaces, e := net.Interfaces()
	if e != nil {
		return e
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ip, _, e := net.ParseCIDR(a.String())
			if e == nil && ip.To4() != nil {
				fmt.Printf("%s: %s (index %d)\n", iface.Name, ip, iface.Index)
			}
		}
	}
	fmt.Println("Required: foreground signed iOS app; Apple Devices USB service; session token typed at the hidden prompt; home Wi-Fi IPv4; free TCP80/443 (or explicit HTTP-only mode).")
	fmt.Println("USB link speed, iPhone cellular throughput and Steam adoption cannot be established by this command.")
	return nil
}

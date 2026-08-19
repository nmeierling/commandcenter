// Package openurl launches a URL in the user's default browser.
package openurl

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// Open validates the URL and hands it to the platform opener.
//
// Validation is not cosmetic: this endpoint takes a URL from an HTTP request
// and passes it to a system command, so anything but http(s) to a local host is
// rejected. Without that, `POST /open` with a "file://" or "javascript:" URL
// would be a way to make the agent act on the user's behalf.
func Open(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("refusing to open %q: only http and https URLs are allowed", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("refusing to open %q: no host", raw)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	name, args := command(u.String())
	if name == "" {
		return fmt.Errorf("opening URLs is not supported on %s", runtime.GOOS)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, out)
	}
	return nil
}

func command(u string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{u}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", u}
	default:
		return "xdg-open", []string{u}
	}
}

// Available reports whether a browser opener exists on this system. On a
// headless Linux box xdg-open is often missing, and the UI should say so rather
// than fail silently.
func Available() bool {
	name, _ := command("http://localhost")
	if name == "" {
		return false
	}
	_, err := exec.LookPath(name)
	return err == nil
}

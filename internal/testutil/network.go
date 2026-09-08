package testutil

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

const resolverEnteredMarker = "resolver-entered"

// RunResolverCancellationProcess runs the current test in child mode, waits
// for its resolver to block, then closes stdin to request cancellation.
func RunResolverCancellationProcess(t *testing.T, childEnv string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(stdout)
	marker := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			readErr <- err
			return
		}
		marker <- strings.TrimSpace(line)
		_, _ = io.Copy(io.Discard, reader)
	}()
	select {
	case got := <-marker:
		if got != resolverEnteredMarker {
			cancel()
			_ = cmd.Wait()
			t.Fatalf("resolver child marker = %q, want %s", got, resolverEnteredMarker)
		}
	case err := <-readErr:
		cancel()
		_ = cmd.Wait()
		t.Fatalf("read resolver child marker: %v\n%s", err, stderr.String())
	case <-ctx.Done():
		_ = cmd.Wait()
		t.Fatalf("resolver child did not enter lookup: %v\n%s", ctx.Err(), stderr.String())
	}
	if err := stdin.Close(); err != nil {
		cancel()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("resolver child failed: %v\n%s", err, stderr.String())
	}
}

type UDPBindCase struct {
	Name     string
	Address  string
	Optional bool
}

// UDPLiteralBindCases returns the literal and wildcard addresses whose legacy
// ListenUDP behavior is covered at both server bind boundaries.
func UDPLiteralBindCases(t *testing.T) []UDPBindCase {
	t.Helper()
	cases := []UDPBindCase{
		{Name: "IPv4", Address: "127.0.0.1:0"},
		{Name: "IPv6", Address: "[::1]:0", Optional: true},
		{Name: "IPv6 wildcard", Address: "[::]:0", Optional: true},
		{Name: "IPv6 wildcard signed port", Address: "[::]:+0", Optional: true},
		{Name: "empty host", Address: ":0"},
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			return append(cases, UDPBindCase{
				Name:     "IPv6 zone",
				Address:  "[::1%" + iface.Name + "]:0",
				Optional: true,
			})
		}
	}
	return cases
}

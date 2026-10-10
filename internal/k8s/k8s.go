// Package k8s deploys kenbun and the eBPF profiler to a cluster by piping
// embedded manifests to kubectl, so it follows the user's kubeconfig and
// contexts without pulling in client-go.
package k8s

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

var (
	//go:embed kenbun.yaml
	kenbunYAML string
	//go:embed profiler.yaml
	profilerYAML string
)

const (
	namespace = "kenbun"
	// Placeholders in the manifests, replaced by Manifests.
	kenbunImage   = "image: kenbun:dev"
	profilerImage = "otel/opentelemetry-collector-ebpf-profiler:0.162.0"
)

type Options struct {
	Context  string // kubeconfig context; "" = current
	Image    string // kenbun image
	Profiler bool   // also deploy the eBPF profiler DaemonSet
}

// Manifests returns everything deploy applies, as one YAML stream.
func Manifests(o Options) (string, error) {
	k, err := replace(kenbunYAML, kenbunImage, "image: "+o.Image)
	if err != nil || !o.Profiler {
		return k, err
	}
	p, err := profilerManifest()
	return k + "---\n" + p, err
}

func profilerManifest() (string, error) {
	v, err := collectorVersion()
	if err != nil {
		return "", err
	}
	return replace(profilerYAML, profilerImage, "otel/opentelemetry-collector-ebpf-profiler:"+v)
}

func replace(s, old, new string) (string, error) {
	if !strings.Contains(s, old) {
		return "", fmt.Errorf("manifest placeholder %q not found", old)
	}
	return strings.ReplaceAll(s, old, new), nil
}

// collectorVersion is the otelcol module version compiled into kenbun. The
// profiler must match it: the profiles proto is Alpha and breaks between releases.
func collectorVersion() (string, error) {
	bi, ok := debug.ReadBuildInfo()
	if ok {
		for _, d := range bi.Deps {
			if d.Path == "go.opentelemetry.io/collector/otelcol" {
				return strings.TrimPrefix(d.Version, "v"), nil
			}
		}
	}
	return "", errors.New("collector version not found in build info")
}

func Deploy(ctx context.Context, o Options) error {
	m, err := Manifests(o)
	if err != nil {
		return err
	}
	if err := kubectl(ctx, o, m, "apply", "-f", "-"); err != nil {
		return err
	}
	if !o.Profiler { // apply doesn't prune: remove one left by an earlier deploy
		p, err := profilerManifest()
		if err != nil {
			return err
		}
		if err := kubectl(ctx, o, p, "delete", "--ignore-not-found", "-f", "-"); err != nil {
			return err
		}
	}
	return kubectl(ctx, o, "", "-n", namespace, "rollout", "status", "deploy/kenbun", "--timeout=2m")
}

func Destroy(ctx context.Context, o Options) error {
	o.Profiler = true // remove everything deploy may have created
	m, err := Manifests(o)
	if err != nil {
		return err
	}
	if err := kubectl(ctx, o, m, "delete", "--ignore-not-found", "--wait=false", "-f", "-"); err != nil {
		return err
	}
	// Wait so an immediate redeploy doesn't hit a terminating namespace, but say
	// so: kubectl delete waits silently.
	fmt.Println("waiting for namespace kenbun to terminate…")
	return kubectl(ctx, o, "", "wait", "--for=delete", "namespace/"+namespace, "--timeout=2m")
}

func kubectl(ctx context.Context, o Options, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", append(contextArgs(o), args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func contextArgs(o Options) []string {
	if o.Context == "" {
		return nil
	}
	return []string{"--context", o.Context}
}

var forwarding = regexp.MustCompile(`Forwarding from (127\.0\.0\.1:\d+)`)

// PortForward forwards a free local port to the kenbun API and returns its
// address. stop ends the forward.
func PortForward(ctx context.Context, o Options) (addr string, stop func(), err error) {
	args := append(contextArgs(o), "-n", namespace, "port-forward", "svc/kenbun", ":7070")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr // not the terminal: the TUI owns it
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop = func() { cmd.Process.Kill(); cmd.Wait() }

	found, exited := make(chan string, 1), make(chan struct{})
	go func() {
		defer close(exited)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := forwarding.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
		io.Copy(io.Discard, out) // keep draining "Handling connection" lines
	}()

	select {
	case addr := <-found:
		return addr, stop, nil
	case <-exited:
	case <-time.After(15 * time.Second):
	}
	stop()
	return "", nil, fmt.Errorf("kubectl port-forward: %s (did you run 'kenbun k8s deploy'?)", strings.TrimSpace(stderr.String()))
}

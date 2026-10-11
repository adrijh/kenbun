package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Deployed is what an earlier 'kenbun k8s deploy' left running.
type Deployed struct {
	Image     string
	Ready     bool
	Started   time.Time // the kenbun pod's start; zero if none runs
	Nodes     int       // nodes the profiler should run on; 0 without it
	Profiling int       // of those, where it's ready
}

// Status reads the kenbun namespace; nil when kenbun isn't deployed.
func Status(ctx context.Context, o Options) (*Deployed, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append(contextArgs(o), "-n", namespace, "get", "deployments,daemonsets,pods", "-o", "json")...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl get: %w", err)
	}
	var list struct {
		Items []struct {
			Kind     string
			Metadata struct {
				Name   string
				Labels map[string]string
			}
			Spec struct {
				Template struct {
					Spec struct{ Containers []struct{ Image string } }
				}
			}
			Status struct {
				ReadyReplicas          int
				DesiredNumberScheduled int
				NumberReady            int
				StartTime              time.Time
			}
		}
	}
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		return nil, fmt.Errorf("kubectl get: %w", err)
	}

	var d *Deployed
	for _, it := range list.Items {
		if it.Kind == "Deployment" && it.Metadata.Name == "kenbun" && len(it.Spec.Template.Spec.Containers) > 0 {
			d = &Deployed{Image: it.Spec.Template.Spec.Containers[0].Image, Ready: it.Status.ReadyReplicas > 0}
		}
	}
	if d == nil {
		return nil, nil
	}
	for _, it := range list.Items {
		switch {
		case it.Kind == "DaemonSet" && it.Metadata.Name == "kenbun-profiler":
			d.Nodes, d.Profiling = it.Status.DesiredNumberScheduled, it.Status.NumberReady
		case it.Kind == "Pod" && it.Metadata.Labels["app.kubernetes.io/name"] == "kenbun" && it.Status.StartTime.After(d.Started):
			d.Started = it.Status.StartTime
		}
	}
	return d, nil
}

// Health is a short account: "up 3h, profiler on 2/2 nodes".
func (d *Deployed) Health() string {
	s := "not ready"
	if d.Ready {
		s = "up " + age(time.Since(d.Started))
	}
	if d.Nodes > 0 {
		s += fmt.Sprintf(", profiler on %d/%d nodes", d.Profiling, d.Nodes)
	}
	return s
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Compare orders two images of kenbun by their tags: -1 if a is older than b,
// 0 if the same image, 1 if newer. ok is false when they can't be ordered
// (another repository, dev builds like v0.0.1-3-gabc).
func Compare(a, b string) (cmp int, ok bool) {
	if a == b {
		return 0, true
	}
	ra, ta, _ := strings.Cut(a, ":")
	rb, tb, _ := strings.Cut(b, ":")
	va, okA := semver(ta)
	vb, okB := semver(tb)
	if ra != rb || !okA || !okB {
		return 0, false
	}
	for i := range va {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// semver parses a release tag, vX.Y.Z; dev builds (with a -suffix) don't parse.
func semver(tag string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// Restart restarts kenbun and its profiler on the same image.
func Restart(ctx context.Context, o Options) error {
	if err := kubectl(ctx, o, "", "-n", namespace, "rollout", "restart", "deployment/kenbun"); err != nil {
		return err
	}
	_ = kubectl(ctx, o, "", "-n", namespace, "rollout", "restart", "daemonset/kenbun-profiler") // absent with --profiler=false
	return kubectl(ctx, o, "", "-n", namespace, "rollout", "status", "deploy/kenbun", "--timeout=2m")
}

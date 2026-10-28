package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// kubectl runs kubectl against one context. orchestrator.Kube keeps its
// own helper unexported, and has no call for what the C0 tool reads (the
// stress-ng replicas, the HPA, the pod's restarts), so this is a second,
// smaller one.
type kubectl struct {
	context string
}

// run returns kubectl's trimmed stdout; stdin may be nil. A failed run's
// error carries the trimmed stderr.
func (k kubectl) run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	if k.context == "" {
		return "", errors.New("c0: kubectl: empty kube context")
	}
	args = append(append([]string(nil), args...), "--request-timeout", "30s", "--context", k.context)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("kubectl %s: %w", args[0], ctx.Err())
		}
		return "", fmt.Errorf("kubectl %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// The part of kubectl's JSON the tool reads.
type podList struct {
	Items []struct {
		Metadata struct {
			Name              string  `json:"name"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		} `json:"metadata"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				RestartCount int `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type hpa struct {
	Spec struct {
		MinReplicas *int `json:"minReplicas"`
		MaxReplicas int  `json:"maxReplicas"`
	} `json:"spec"`
	Status struct {
		CurrentReplicas int `json:"currentReplicas"`
		DesiredReplicas int `json:"desiredReplicas"`
	} `json:"status"`
}

// podState is what the checks need of the variant's pods.
type podState struct {
	// Pods counts the pods not terminating; Ready how many are Ready.
	Pods, Ready int
	// Name and Restarts are the single pod's, set when Pods == 1.
	Name     string
	Restarts int
}

// pods reads the hasher pods of namespace ns.
func (k kubectl) pods(ctx context.Context, ns string) (podState, error) {
	out, err := k.run(ctx, nil, "get", "pods", "-n", ns, "-l", "app=hasher", "-o", "json")
	if err != nil {
		return podState{}, err
	}
	var l podList
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return podState{}, fmt.Errorf("c0: decoding pods: %w", err)
	}
	var s podState
	for _, p := range l.Items {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		s.Pods++
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" && p.Status.Phase == "Running" {
				s.Ready++
			}
		}
		s.Name = p.Metadata.Name
		s.Restarts = 0
		for _, c := range p.Status.ContainerStatuses {
			s.Restarts += c.RestartCount
		}
	}
	if s.Pods != 1 {
		s.Name, s.Restarts = "", 0
	}
	return s, nil
}

// hpaPinned reports whether the HPA holds the variant at exactly one
// replica: min = max = 1, and current and desired 1.
func (k kubectl) hpaPinned(ctx context.Context, ns string) (bool, string, error) {
	out, err := k.run(ctx, nil, "get", "hpa", "hasher", "-n", ns, "-o", "json")
	if err != nil {
		return false, "", err
	}
	var h hpa
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		return false, "", fmt.Errorf("c0: decoding hpa: %w", err)
	}
	min := 1 // the API server defaults minReplicas to 1
	if h.Spec.MinReplicas != nil {
		min = *h.Spec.MinReplicas
	}
	if min != 1 || h.Spec.MaxReplicas != 1 || h.Status.CurrentReplicas != 1 || h.Status.DesiredReplicas != 1 {
		return false, fmt.Sprintf("hpa min %d max %d current %d desired %d",
			min, h.Spec.MaxReplicas, h.Status.CurrentReplicas, h.Status.DesiredReplicas), nil
	}
	return true, "", nil
}

// waitOnePod polls until exactly one hasher pod exists, not terminating
// and Ready, at most bound.
func (k kubectl) waitOnePod(ctx context.Context, ns string, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		s, err := k.pods(ctx, ns)
		if err != nil {
			return err
		}
		if s.Pods == 1 && s.Ready == 1 {
			return nil
		}
		if time.Now().Add(time.Second).After(deadline) {
			return fmt.Errorf("c0: %s: %d pods, %d Ready after %v, want exactly one Ready", ns, s.Pods, s.Ready, bound)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

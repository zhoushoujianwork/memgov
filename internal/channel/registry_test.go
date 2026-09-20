package channel

import (
	"context"
	"testing"

	"github.com/zhoushoujianwork/memgov/internal/core"
)

type registryTestAdapter struct{ name string }

func (a *registryTestAdapter) Name() string { return a.name }
func (*registryTestAdapter) ProbeCapabilities(context.Context, Config) (core.Capabilities, error) {
	return core.Capabilities{Verified: map[string]bool{}}, nil
}
func (*registryTestAdapter) ReadWindow(context.Context, Config, Window) (WindowResult, error) {
	return WindowResult{Complete: true}, nil
}
func (*registryTestAdapter) RunReceiver(context.Context, Config, ReceiverOptions) error { return nil }
func (*registryTestAdapter) Send(context.Context, Config, SendRequest) (SendResult, error) {
	return SendResult{State: "accepted"}, nil
}

func TestRegistryResolvesIndependentPlatformFactories(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("matrix", func() Adapter { return &registryTestAdapter{name: "matrix"} }); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("slack", func() Adapter { return &registryTestAdapter{name: "slack"} }); err != nil {
		t.Fatal(err)
	}
	matrixFactory, ok := r.Lookup(" matrix ")
	if !ok {
		t.Fatal("matrix adapter was not registered")
	}
	slackFactory, ok := r.Lookup("slack")
	if !ok {
		t.Fatal("slack adapter was not registered")
	}
	if matrixFactory().Name() != "matrix" || slackFactory().Name() != "slack" {
		t.Fatal("registry resolved the wrong platform factory")
	}
	if _, ok := r.Lookup("unknown"); ok {
		t.Fatal("unknown platform unexpectedly resolved")
	}
}

func TestRegistryRejectsDuplicateAndAllowsExplicitReplacement(t *testing.T) {
	r := NewRegistry()
	factory := func() Adapter { return &registryTestAdapter{name: "first"} }
	if err := r.Register("chat", factory); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("chat", factory); core.ErrorCode(err) != "conflict" {
		t.Fatalf("duplicate registration error=%v, want conflict", err)
	}
	if err := r.Replace("chat", func() Adapter { return &registryTestAdapter{name: "replacement"} }); err != nil {
		t.Fatal(err)
	}
	resolved, ok := r.Lookup("chat")
	if !ok || resolved().Name() != "replacement" {
		t.Fatal("explicit replacement was not applied")
	}
}

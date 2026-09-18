package runtime

import (
	"context"
	"testing"
)

func TestIdleTickDoesNotOpenWriteTransactions(t *testing.T) {
	service, cfg, preset, _, _ := setupService(t)
	ctx := context.Background()
	var before, after int
	if err := service.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM requests").Scan(&before); err != nil {
		t.Fatal(err)
	}
	service.tick(ctx, cfg, preset)
	service.tick(ctx, cfg, preset)
	if err := service.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM requests").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("idle ticks wrote %d request records", after-before)
	}
}

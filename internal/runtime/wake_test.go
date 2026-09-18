package runtime

import "testing"

func TestWakeBusBroadcastsAndCoalescesCommittedIntake(t *testing.T) {
	bus := NewWakeBus()
	one, stopOne := bus.Subscribe("app")
	two, stopTwo := bus.Subscribe("app")
	other, stopOther := bus.Subscribe("other")
	defer stopTwo()
	defer stopOther()

	bus.Notify("app")
	bus.Notify("app")
	for name, ch := range map[string]<-chan struct{}{"one": one, "two": two} {
		select {
		case <-ch:
		default:
			t.Fatalf("%s subscriber was not woken", name)
		}
		select {
		case <-ch:
			t.Fatalf("%s subscriber received an uncoalesced duplicate wake", name)
		default:
		}
	}
	select {
	case <-other:
		t.Fatal("unrelated channel subscriber was woken")
	default:
	}

	stopOne()
	bus.Notify("app")
	select {
	case <-one:
		t.Fatal("unsubscribed runtime was woken")
	default:
	}
}

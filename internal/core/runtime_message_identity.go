package core

import "unicode/utf8"

// Names are bounded display metadata from this message's source header. They
// never replace a stable sender principal or authorize another identity lookup.
func runtimeSenderDisplayName(snapshot string) string {
	name := messageSnapshotField(snapshot, "sender_display_name")
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 {
		return ""
	}
	return name
}

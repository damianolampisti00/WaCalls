package signaling

import (
	"bytes"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

func kids(n waBinary.Node) []waBinary.Node { return n.GetChildren() }

func inner(n waBinary.Node) waBinary.Node { return kids(n)[0] }

func childTags(n waBinary.Node) []string {
	var out []string
	for _, c := range kids(n) {
		out = append(out, c.Tag)
	}
	return out
}

func TestPreacceptVideoShape(t *testing.T) {
	peer := types.NewJID("393331234567", types.DefaultUserServer)
	defer func(v bool) { VideoSignaling = v }(VideoSignaling)

	VideoSignaling = false
	legacy := inner(BuildPreacceptStanza(peer, "CID", peer, true))
	if got := childTags(legacy); len(got) != 3 || got[0] != "audio" || got[1] != "encopt" || got[2] != "capability" {
		t.Fatalf("legacy video preaccept must be unchanged, got %v", got)
	}

	VideoSignaling = true
	pre := inner(BuildPreacceptStanza(peer, "CID", peer, true))
	got := childTags(pre)
	if len(got) != 4 || got[0] != "audio" || got[1] != "video" || got[2] != "encopt" || got[3] != "capability" {
		t.Fatalf("order audio -> video -> encopt -> capability, got %v", got)
	}
	v := kids(pre)[1]
	if v.Attrs["dec"] != "H264" || v.Attrs["device_orientation"] != "0" || v.Attrs["screen_width"] != "0" || v.Attrs["screen_height"] != "0" {
		t.Fatalf("video preaccept attrs %v", v.Attrs)
	}
	if _, ok := v.Attrs["enc"]; ok {
		t.Fatal("a preaccept carries no enc")
	}
	if c, _ := kids(pre)[3].Content.([]byte); !bytes.Equal(c, capabilityVideoPreaccept) {
		t.Fatalf("capability % x", c)
	}
	// An audio call keeps the audio preaccept even with the flag on.
	audio := inner(BuildPreacceptStanza(peer, "CID", peer, false))
	if c, _ := kids(audio)[2].Content.([]byte); len(kids(audio)) != 3 || !bytes.Equal(c, capabilityPreaccept) {
		t.Fatalf("audio preaccept changed: %v", childTags(audio))
	}
}

func TestVideoOfferNodeSpelling(t *testing.T) {
	n := videoOfferNode()
	if n.Attrs["enc"] != "h.264" || n.Attrs["dec"] != "H264" || n.Attrs["screen_width"] != "0" {
		t.Fatalf("%v", n.Attrs)
	}
	if _, ok := n.Attrs["orientation"]; ok {
		t.Fatal("'orientation' is not in WhatsApp's <video> grammar")
	}
}

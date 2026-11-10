package agent

import (
	"encoding/base64"
	"io"
	"strings"

	"filippo.io/age/armor"
)

func encodeStd(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// armorReader accepts an ASCII-armored age file.
func armorReader(s string) io.Reader { return armor.NewReader(strings.NewReader(s)) }

package airplay

import (
	"strconv"
	"strings"
)

// v0.5.3's structured MRP contract. Never forward arbitrary engine strings:
// reasons are enums, dimensions/codes are bounded integers, paths are enums.
// posted/HTTP 200 describes protocol delivery, not visible TV artwork.
func artworkStatusDiagnostic(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "mrp" {
		return ""
	}
	values := make(map[string]string)
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return "" // ambiguous classifications are not trustworthy
		}
		values[key] = value
	}
	var result string
	switch {
	case values["artwork"] == "rejected":
		result = "mrp artwork=rejected reason="
		switch reason := values["reason"]; reason {
		case "invalid_argument", "unsupported_type", "staging_limit", "invalid_jpeg_envelope", "no_memory", "invalid_artwork":
			result += reason
		default:
			result += "unknown"
		}
	case values["artwork"] == "posted":
		result = "mrp artwork=posted"
	case values["path"] == "command", values["path"] == "channel":
		result = "mrp path=" + values["path"]
	default:
		return ""
	}
	for _, key := range []string{"status", "clear_status", "bytes", "width", "height", "precision", "components", "progressive", "staging_max_bytes"} {
		value, err := strconv.ParseInt(values[key], 10, 32)
		if err != nil || value < -1 || ((key == "status" || key == "clear_status") && value > 599) || (key != "status" && key != "clear_status" && value < 0) {
			continue
		}
		result += " " + key + "=" + strconv.FormatInt(value, 10)
	}
	return result
}

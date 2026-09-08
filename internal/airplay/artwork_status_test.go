package airplay

import "testing"

func TestArtworkStatusPreservesSafeProtocolDetails(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"mrp path=command status=200", "mrp path=command status=200"},
		{"mrp path=channel status=-1 token=secret", "mrp path=channel status=-1"},
		{"mrp artwork=posted bytes=123 width=512 height=256 status=200", "mrp artwork=posted status=200 bytes=123 width=512 height=256"},
		{"mrp artwork=rejected reason=staging_limit bytes=1048577 clear_status=500 url=secret", "mrp artwork=rejected reason=staging_limit clear_status=500 bytes=1048577"},
		{"mrp artwork=rejected reason=invalid_jpeg_envelope precision=8 progressive=1", "mrp artwork=rejected reason=invalid_jpeg_envelope precision=8 progressive=1"},
		{"mrp artwork=rejected reason=secret path=/private bytes=999999999999999999999 width=secret status=secret", "mrp artwork=rejected reason=unknown"},
		{"mrp artwork=posted status=200 status=500", ""},
		{"mrp path=/private status=200", ""},
		{"mrp artwork=other reason=secret", ""},
	} {
		if got := artworkStatusDiagnostic(tc.line); got != tc.want {
			t.Fatalf("diagnostic = %q, want %q", got, tc.want)
		}
	}
}

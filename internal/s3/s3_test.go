package s3

import (
	"net/http"
	"testing"
)

// The example from AWS's Signature Version 4 documentation for S3
// ("Example: GET Object"), with its published signature.
func TestSignatureV4Example(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("x-amz-content-sha256", emptySHA256)
	req.Header.Set("x-amz-date", "20130524T000000Z")
	got := authorization(req, emptySHA256, "20130524T000000Z", "us-east-1",
		"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

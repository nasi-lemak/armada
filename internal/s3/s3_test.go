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

// More of AWS's published S3 examples: an upload (a "$" in the key, an
// extra signed header), a sub-resource with an empty value, and a listing
// with query parameters.
func TestSignatureV4MoreExamples(t *testing.T) {
	const (
		ak   = "AKIAIOSFODNN7EXAMPLE"
		sk   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date = "20130524T000000Z"
	)
	for _, c := range []struct {
		name, method, url, hash, signed, sig string
		headers                              map[string]string
	}{
		{"put object", "PUT", "https://examplebucket.s3.amazonaws.com/test$file.text",
			"44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
			map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "x-amz-storage-class": "REDUCED_REDUNDANCY"}},
		{"get lifecycle", "GET", "https://examplebucket.s3.amazonaws.com/?lifecycle", emptySHA256,
			"host;x-amz-content-sha256;x-amz-date",
			"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543", nil},
		{"list objects", "GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", emptySHA256,
			"host;x-amz-content-sha256;x-amz-date",
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7", nil},
	} {
		req, _ := http.NewRequest(c.method, c.url, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("x-amz-content-sha256", c.hash)
		req.Header.Set("x-amz-date", date)
		got := authorization(req, c.hash, date, "us-east-1", ak, sk)
		want := "AWS4-HMAC-SHA256 Credential=" + ak + "/20130524/us-east-1/s3/aws4_request, SignedHeaders=" + c.signed + ", Signature=" + c.sig
		if got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
	}
}

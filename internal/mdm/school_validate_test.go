package mdm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// secretMarker stands in for the credential material a real School profile (or a
// Jamf error page) carries. Every rejection test embeds it so we can assert the
// returned error never quotes its input back at the caller.
const secretMarker = "s3cr3t-marker-do-not-log"

// syntheticSignedDataCMS builds a DER ContentInfo carrying the pkcs7-signedData
// OID, with a throwaway self-signed certificate as its content. It is generated
// here rather than copied from a real download: a real School profile embeds a
// live PKCS#12 identity and its cleartext password, so it must never become a
// test fixture.
func syntheticSignedDataCMS(t *testing.T) []byte {
	t.Helper()
	return syntheticContentInfo(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2})
}

// syntheticEmptyContentInfo builds a 13-byte ContentInfo carrying the signedData
// OID and no content at all — the smallest thing that satisfies a ContentType-only
// check.
func syntheticEmptyContentInfo(t *testing.T) []byte {
	t.Helper()
	der, err := asn1.Marshal(struct {
		ContentType asn1.ObjectIdentifier
	}{ContentType: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}})
	if err != nil {
		t.Fatalf("marshal empty ContentInfo: %v", err)
	}
	return der
}

// syntheticHugeOIDContentInfo builds a ContentInfo whose content type is a
// thousands-of-arcs OID, to prove an error message cannot be inflated by its input.
func syntheticHugeOIDContentInfo(t *testing.T) []byte {
	t.Helper()
	oid := make(asn1.ObjectIdentifier, 2000)
	oid[0], oid[1] = 1, 2
	for index := 2; index < len(oid); index++ {
		oid[index] = 999999
	}
	return syntheticContentInfo(t, oid)
}

func syntheticContentInfo(t *testing.T, contentType asn1.ObjectIdentifier) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate throwaway key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(4242),
		Subject:      pkix.Name{CommonName: "synthetic-test-signer.example.invalid"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create throwaway certificate: %v", err)
	}
	// A stand-in for SignedData: enough structure that the content is a real
	// explicit [0] wrapper holding a SEQUENCE, which is what the validator's
	// non-empty-content check is about. It is not a verifiable signature, and does
	// not need to be — nothing here verifies one.
	der, err := asn1.Marshal(struct {
		ContentType asn1.ObjectIdentifier
		Content     struct {
			Version     int
			Certificate asn1.RawValue
		} `asn1:"explicit,tag:0"`
	}{
		ContentType: contentType,
		Content: struct {
			Version     int
			Certificate asn1.RawValue
		}{Version: 1, Certificate: asn1.RawValue{FullBytes: certDER}},
	})
	if err != nil {
		t.Fatalf("marshal ContentInfo: %v", err)
	}
	return der
}

const schoolXMLPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadType</key><string>Configuration</string>
	<key>PayloadIdentifier</key><string>com.apple.mdm</string>
	<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000001</string>
	<key>PayloadVersion</key><integer>1</integer>
	<key>PayloadDisplayName</key><string>Fake Enrollment Profile</string>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>PayloadType</key><string>com.apple.security.pkcs12</string>
			<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000002</string>
			<key>Password</key><string>` + secretMarker + `</string>
		</dict>
		<dict>
			<key>PayloadType</key><string>com.apple.mdm</string>
			<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000003</string>
			<key>ServerURL</key><string>https://mdm.example.invalid/mdm</string>
			<key>CheckInURL</key><string>https://mdm.example.invalid/checkin</string>
			<key>Topic</key><string>com.apple.mgmt.External.00000000-0000-4000-8000-000000000004</string>
		</dict>
	</array>
</dict>
</plist>
`

func TestValidateSchoolProfileAcceptsSyntheticSignedCMS(t *testing.T) {
	if err := ValidateSchoolProfile(syntheticSignedDataCMS(t)); err != nil {
		t.Fatalf("ValidateSchoolProfile rejected a signed CMS profile: %v", err)
	}
}

func TestValidateSchoolProfileAcceptsUnsignedXMLPlist(t *testing.T) {
	if err := ValidateSchoolProfile([]byte(schoolXMLPlist)); err != nil {
		t.Fatalf("ValidateSchoolProfile rejected an unsigned XML plist profile: %v", err)
	}
}

func TestValidateSchoolProfileAcceptsLeadingWhitespaceBeforeXML(t *testing.T) {
	if err := ValidateSchoolProfile([]byte("\n  " + schoolXMLPlist)); err != nil {
		t.Fatalf("ValidateSchoolProfile rejected an XML plist with leading whitespace: %v", err)
	}
}

func schoolProfileRejectionCases(t *testing.T) []struct {
	name  string
	input []byte
} {
	t.Helper()
	oversized := make([]byte, maxSchoolProfileBytes+1)
	copy(oversized, syntheticSignedDataCMS(t))
	copy(oversized[len(oversized)-len(secretMarker):], secretMarker)

	truncated := syntheticSignedDataCMS(t)
	truncated = truncated[:len(truncated)/2]

	withTrailingBytes := append(syntheticSignedDataCMS(t), []byte(secretMarker)...)

	return []struct {
		name  string
		input []byte
	}{
		{"empty", nil},
		{"whitespace only", []byte("   \n\t  ")},
		{"html doctype login page", []byte("<!DOCTYPE html>\n<html><body>Sign in. xml plist " + secretMarker + "</body></html>")},
		{"bare html tag", []byte("<html><head><title>" + secretMarker + "</title></head></html>")},
		{"html with leading whitespace", []byte("\n\n  <!doctype HTML>\n<html>" + secretMarker + "</html>")},
		{"json error body", []byte(`{"error":"session expired","detail":"` + secretMarker + `"}`)},
		{"json with leading whitespace", []byte("  \n" + `{"profile":"` + secretMarker + `"}`)},
		{"oversized", oversized},
		{"truncated der", truncated},
		{"signed data with trailing bytes", withTrailingBytes},
		{"content info with wrong oid", syntheticContentInfo(t, asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1})},
		{"plain text", []byte("not a profile at all: " + secretMarker)},
		{"xml plist with pro profile service type", []byte(strings.Replace(
			strings.Replace(schoolXMLPlist, "<string>Configuration</string>", "<string>Profile Service</string>", 1),
			"com.apple.mdm", "com.apple.notmdm", -1))},
		{"xml plist missing payload uuid", []byte(strings.Replace(schoolXMLPlist,
			"<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000001</string>", "", 1))},
		{"xml plist with empty payload uuid", []byte(strings.Replace(schoolXMLPlist,
			"<string>00000000-0000-4000-8000-000000000001</string>", "<string></string>", 1))},
		{"xml plist without an mdm payload", []byte(strings.Replace(schoolXMLPlist,
			"<key>PayloadType</key><string>com.apple.mdm</string>\n\t\t\t<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000003</string>",
			"<key>PayloadType</key><string>com.apple.wifi.managed</string>\n\t\t\t<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000003</string>", 1))},
		{"xml plist with no payload content", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>PayloadType</key><string>Configuration</string>
<key>PayloadUUID</key><string>00000000-0000-4000-8000-000000000001</string>
</dict></plist>`)},
		{"xml with a non-plist root", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<response><error>` + secretMarker + `</error></response>`)},
		{"xml plist containing malformed markup", []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>PayloadType</key><string>Configuration</dict></plist>`)},
		// A ContentType-only check would pass this 13-byte blob, and it would then be
		// copied to a guest's Desktop as "the enrollment profile".
		{"signed data content info with no content", syntheticEmptyContentInfo(t)},
		// The DER branch rejects trailing bytes; the XML branch must not be more
		// lenient, or a profile with a login page stapled to it sails through.
		{"xml plist with a login page appended", []byte(schoolXMLPlist + "<!DOCTYPE html><html>Sign in " + secretMarker + "</html>")},
		{"xml plist with binary garbage appended", append([]byte(schoolXMLPlist), 0x00, 0x01, 0x02, 'g', 'a', 'r', 'b')},
		{"xml plist with no closing plist tag", []byte(strings.Replace(schoolXMLPlist, "</plist>", "", 1))},
		{"xml plist with a second plist appended", []byte(schoolXMLPlist + schoolXMLPlist)},
	}
}

// Finding from review: an OID is parsed out of the response, so interpolating it
// lets the input choose the error's length. A 2000-arc OID inflated the message to
// 16 KB, which would land in task output and therefore in the SSE snapshot.
func TestValidateSchoolProfileErrorsCannotBeInflatedByTheirInput(t *testing.T) {
	err := ValidateSchoolProfile(syntheticHugeOIDContentInfo(t))
	if err == nil {
		t.Fatalf("ValidateSchoolProfile accepted a content info with a 2000-arc OID")
	}
	if length := len(err.Error()); length > 256 {
		t.Fatalf("error message is %d bytes; input must not be able to size it", length)
	}
	if strings.Contains(err.Error(), "999999") {
		t.Fatalf("error interpolated the OID parsed out of the input: %q", err.Error())
	}
}

// The plan mandates this specific diagnostic: pointing a School fetch at a Jamf
// Pro profile is a configuration mistake a user can act on, so it must not fall
// through to the generic "PayloadType is not Configuration" message.
func TestValidateSchoolProfileNamesAProProfileAsSuch(t *testing.T) {
	proProfile := strings.Replace(schoolXMLPlist, "<string>Configuration</string>", "<string>Profile Service</string>", 1)
	err := ValidateSchoolProfile([]byte(proProfile))
	if err == nil {
		t.Fatalf("ValidateSchoolProfile accepted a Jamf Pro profile")
	}
	if !strings.Contains(err.Error(), "Jamf Pro") {
		t.Fatalf("error does not identify the profile as a Jamf Pro one: %q", err.Error())
	}
}

// An unsigned XML plist is a format the validator exists to accept, so a byte
// order mark must not divert it into the ASN.1 branch and fail it with a
// misleading message.
func TestValidateSchoolProfileAcceptsABOMPrefixedXMLPlist(t *testing.T) {
	if err := ValidateSchoolProfile(append([]byte("\xef\xbb\xbf"), schoolXMLPlist...)); err != nil {
		t.Fatalf("ValidateSchoolProfile rejected a BOM-prefixed XML plist: %v", err)
	}
}

// Whitespace and comments after </plist> are well-formed XML and must stay
// acceptable, so that rejecting trailing content does not over-reach.
func TestValidateSchoolProfileAcceptsTrailingWhitespaceAndComments(t *testing.T) {
	if err := ValidateSchoolProfile([]byte(schoolXMLPlist + "\n<!-- generated -->\n\n")); err != nil {
		t.Fatalf("ValidateSchoolProfile rejected trailing whitespace and comments: %v", err)
	}
}

func TestValidateSchoolProfileRejectsNonProfiles(t *testing.T) {
	for _, testCase := range schoolProfileRejectionCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			if err := ValidateSchoolProfile(testCase.input); err == nil {
				t.Fatalf("ValidateSchoolProfile accepted %s", testCase.name)
			}
		})
	}
}

// A School profile is a credential: it embeds a PKCS#12 identity and that
// identity's cleartext password. Validation errors surface in task output and in
// HTTP responses, so they must describe the shape of the failure and never quote
// the bytes.
func TestValidateSchoolProfileErrorsNeverQuoteTheirInput(t *testing.T) {
	for _, testCase := range schoolProfileRejectionCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateSchoolProfile(testCase.input)
			if err == nil {
				t.Fatalf("ValidateSchoolProfile accepted %s", testCase.name)
			}
			message := err.Error()
			if strings.Contains(message, secretMarker) {
				t.Fatalf("error quoted the input back: %q", message)
			}
			for _, fragment := range []string{"<html", "<!DOCTYPE", "ServerURL", "Password"} {
				if strings.Contains(message, fragment) {
					t.Fatalf("error leaked input fragment %q: %q", fragment, message)
				}
			}
		})
	}
}

func TestValidateSchoolProfileAcceptsProfileAtExactlyTheSizeCap(t *testing.T) {
	// The padding comment goes *inside* the root dict, so this test measures the
	// cap and nothing else. Padding after </plist> would also have passed by way of
	// trailing-content leniency, quietly locking that leniency in.
	const anchor = "<key>PayloadVersion</key><integer>1</integer>"
	padding := maxSchoolProfileBytes - len(schoolXMLPlist) - len("<!--  -->")
	if padding < 0 {
		t.Fatalf("fixture %d bytes is already larger than the %d byte cap", len(schoolXMLPlist), maxSchoolProfileBytes)
	}
	comment := fmt.Sprintf("<!--%s-->", strings.Repeat("x", padding+2))
	padded := strings.Replace(schoolXMLPlist, anchor, anchor+comment, 1)
	if !strings.Contains(padded, comment) {
		t.Fatalf("padding anchor %q not found in the fixture", anchor)
	}
	if len(padded) != maxSchoolProfileBytes {
		t.Fatalf("padded fixture is %d bytes, want %d", len(padded), maxSchoolProfileBytes)
	}
	if err := ValidateSchoolProfile([]byte(padded)); err != nil {
		t.Fatalf("ValidateSchoolProfile rejected a profile at exactly the size cap: %v", err)
	}
}

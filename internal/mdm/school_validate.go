package mdm

import (
	"bytes"
	"encoding/asn1"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxSchoolProfileBytes caps both every response body read from a Jamf School
// server and the largest profile ValidateSchoolProfile will accept. A real
// download is around 9.7 KB, so 1 MiB is generous headroom while still stopping a
// server that streams an unbounded body from exhausting memory. Precedent for
// capping a read: internal/mcp/mcp.go.
const maxSchoolProfileBytes = 1 << 20

const (
	schoolPayloadTypeConfiguration = "Configuration"
	proPayloadTypeProfileService   = "Profile Service"
	mdmPayloadType                 = "com.apple.mdm"
)

// oidSignedData is PKCS#7 signedData, 1.2.840.113549.1.7.2.
var oidSignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}

// pkcs7ContentInfo is the PKCS#7 outer ContentInfo (RFC 2315 s7). Only the
// ContentType is inspected; the signed content is deliberately left opaque.
type pkcs7ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"optional,explicit,tag:0"`
}

// errSchoolPlistMalformed is returned for every XML syntax or structure fault, so
// no parser message — which could embed the offending bytes — reaches the caller.
var errSchoolPlistMalformed = errors.New("school enrollment profile is not well-formed XML")

// errSchoolPlistTrailingContent is distinct from errSchoolPlistMalformed because
// the input is well-formed XML; the fault is that something follows the profile.
var errSchoolPlistTrailingContent = errors.New("school enrollment profile has content after the end of its plist")

// ValidateSchoolProfile sanity-checks a profile downloaded from a Jamf School
// server.
//
// This check is MUCH WEAKER than the one ValidateProfile applies to Jamf Pro
// profiles, and deliberately so. A Pro profile is generated locally, so every
// value in it can be re-parsed and compared against what we authored. A School
// profile is produced by the server and normally arrives as DER-encoded PKCS#7
// SignedData, which we neither parse nor want to unwrap: macOS installs signed
// .mobileconfig files natively and the install dialog then reads "Verified", so
// unwrapping would be a downgrade. All we can establish here is shape — that
// these bytes are plausibly an enrollment profile rather than the HTML login page
// a Jamf School server serves once an enrollment session expires. Do not read
// this as parity with the Pro path.
//
// Errors describe the shape of the failure and never quote the bytes: a School
// profile embeds a PKCS#12 client identity together with that identity's
// cleartext password, and these errors reach task output and HTTP responses.
func ValidateSchoolProfile(profile []byte) error {
	if len(profile) > maxSchoolProfileBytes {
		return fmt.Errorf("school enrollment profile is %d bytes, over the %d byte limit", len(profile), maxSchoolProfileBytes)
	}
	// A UTF-8 BOM is stripped as well as whitespace: an unsigned XML plist is a
	// format this validator exists to accept, and a BOM would otherwise divert it
	// into the ASN.1 branch and fail it with a misleading message.
	body := bytes.TrimLeft(bytes.TrimPrefix(profile, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(body) == 0 {
		return errors.New("school enrollment profile is empty")
	}

	// An expired enrollment session returns a login page and a refused request
	// returns a JSON error body. These are the failures that actually happen, so
	// name them rather than letting them fall through to the opaque "not a signed
	// profile" message. Note that the bash prototype's `file -b | grep -i
	// "xml\|plist"` check passes on an HTML page that merely contains the word
	// "xml", which is exactly how this failure went unnoticed there.
	prefixLength := min(len(body), 64)
	prefix := bytes.ToLower(body[:prefixLength])
	if bytes.HasPrefix(prefix, []byte("<!doctype html")) || bytes.HasPrefix(prefix, []byte("<html")) {
		return errors.New("school enrollment server returned an HTML page instead of a profile; the enrollment session may have expired")
	}
	if body[0] == '{' || body[0] == '[' {
		return errors.New("school enrollment server returned a JSON body instead of a profile")
	}

	// Format-agnostic by necessity: the live download is DER, but an unsigned XML
	// variant may also appear, so neither branch may assume the other's format.
	if bytes.HasPrefix(prefix, []byte("<?xml")) ||
		bytes.HasPrefix(prefix, []byte("<!doctype plist")) ||
		bytes.HasPrefix(prefix, []byte("<plist")) {
		return validateSchoolXMLPlist(body)
	}
	return validateSchoolSignedProfile(profile)
}

// validateSchoolSignedProfile checks that the bytes are a DER PKCS#7 SignedData
// envelope. The signature itself is not verified — we have no trust anchor for
// the tenant's signer, and macOS verifies it at install time.
func validateSchoolSignedProfile(profile []byte) error {
	var info pkcs7ContentInfo
	rest, err := asn1.Unmarshal(profile, &info)
	if err != nil {
		return errors.New("school enrollment profile is neither an XML plist nor a DER-encoded PKCS#7 structure")
	}
	if len(rest) != 0 {
		return fmt.Errorf("school enrollment profile has %d trailing bytes after its PKCS#7 structure", len(rest))
	}
	if !info.ContentType.Equal(oidSignedData) {
		// The content type is parsed out of the response, so it is not interpolated:
		// an OID carries no credential, but a multi-thousand-arc one would let the
		// input choose this message's length, and it ends up in task output.
		return fmt.Errorf("school enrollment profile is not PKCS#7 signedData (%s)", oidSignedData)
	}
	// Content is optional in the ASN.1 definition, so a 13-byte ContentInfo with the
	// right OID and nothing in it parses cleanly. Without this check that blob would
	// be copied to a guest's Desktop as the enrollment profile.
	if len(info.Content.Bytes) == 0 {
		return errors.New("school enrollment profile is an empty PKCS#7 signedData envelope")
	}
	return nil
}

func validateSchoolXMLPlist(profile []byte) error {
	root, err := decodeSchoolPlist(profile)
	if err != nil {
		return err
	}
	if root.kind != plistDict {
		return errors.New("school enrollment profile plist root value is not a dictionary")
	}
	payloadType, _ := root.stringValue("PayloadType")
	switch payloadType {
	case schoolPayloadTypeConfiguration:
	case proPayloadTypeProfileService:
		return errors.New(`school enrollment profile has PayloadType "Profile Service", which is a Jamf Pro enrollment profile rather than a Jamf School one`)
	default:
		return errors.New(`school enrollment profile PayloadType is not "Configuration"`)
	}
	if payloadUUID, ok := root.stringValue("PayloadUUID"); !ok || strings.TrimSpace(payloadUUID) == "" {
		return errors.New("school enrollment profile has no PayloadUUID")
	}
	if !schoolPlistHasMDMPayload(root.entries["PayloadContent"]) {
		return errors.New(`school enrollment profile PayloadContent has no "com.apple.mdm" payload`)
	}
	return nil
}

func schoolPlistHasMDMPayload(content plistValue) bool {
	switch content.kind {
	case plistDict:
		payloadType, _ := content.stringValue("PayloadType")
		return payloadType == mdmPayloadType
	case plistArray:
		for _, item := range content.items {
			if schoolPlistHasMDMPayload(item) {
				return true
			}
		}
	}
	return false
}

// A minimal XML plist reader. The package already has parseMDMProfile, but that
// one is shaped around the Pro template, where PayloadContent is a single dict; a
// School profile's PayloadContent is an array of payload dicts.

type plistKind int

const (
	plistInvalid plistKind = iota
	plistDict
	plistArray
	plistString
	plistScalar // integer, real, date, data, true, false
)

type plistValue struct {
	kind    plistKind
	text    string
	entries map[string]plistValue
	items   []plistValue
}

func (v plistValue) stringValue(key string) (string, bool) {
	entry, ok := v.entries[key]
	if !ok || entry.kind != plistString {
		return "", false
	}
	return entry.text, true
}

// maxSchoolPlistDepth bounds recursion so a deeply nested body cannot exhaust the
// stack. Real profiles nest three or four levels.
const maxSchoolPlistDepth = 32

func decodeSchoolPlist(profile []byte) (plistValue, error) {
	decoder := xml.NewDecoder(bytes.NewReader(profile))
	for {
		token, err := decoder.Token()
		if err != nil {
			return plistValue{}, errSchoolPlistMalformed
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "plist" {
			return plistValue{}, errors.New("school enrollment profile is XML, but its root element is not a plist")
		}
		root, found, err := decodeSchoolPlistValue(decoder, 0)
		if err != nil {
			return plistValue{}, err
		}
		if !found {
			return plistValue{}, errors.New("school enrollment profile plist is empty")
		}
		if err := requireSchoolPlistEnd(decoder, start.Name); err != nil {
			return plistValue{}, err
		}
		return root, nil
	}
}

// requireSchoolPlistEnd consumes </plist> and then requires the end of the input,
// allowing only whitespace, comments and processing instructions after it.
//
// Without this the XML branch would accept a valid plist with anything at all
// stapled on — an HTML login page, say — and would accept one that was never
// closed. validateSchoolSignedProfile rejects trailing bytes for exactly the same
// reason, and the two branches must not disagree about it.
func requireSchoolPlistEnd(decoder *xml.Decoder, name xml.Name) error {
	closed := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !closed {
				return errSchoolPlistMalformed
			}
			return nil
		}
		if err != nil {
			return errSchoolPlistMalformed
		}
		switch token := token.(type) {
		case xml.EndElement:
			if closed || token.Name != name {
				return errSchoolPlistMalformed
			}
			closed = true
		case xml.CharData:
			if len(bytes.TrimSpace(token)) != 0 {
				return errSchoolPlistTrailingContent
			}
		case xml.StartElement, xml.Directive:
			return errSchoolPlistTrailingContent
		}
	}
}

// decodeSchoolPlistValue reads the next value element inside the current
// container. found is false when the container's end element was reached instead.
func decodeSchoolPlistValue(decoder *xml.Decoder, depth int) (plistValue, bool, error) {
	if depth > maxSchoolPlistDepth {
		return plistValue{}, false, errors.New("school enrollment profile plist is nested too deeply")
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return plistValue{}, false, errSchoolPlistMalformed
		}
		switch token := token.(type) {
		case xml.EndElement:
			return plistValue{}, false, nil
		case xml.StartElement:
			value, err := decodeSchoolPlistElement(decoder, token, depth)
			if err != nil {
				return plistValue{}, false, err
			}
			return value, true, nil
		}
	}
}

func decodeSchoolPlistElement(decoder *xml.Decoder, start xml.StartElement, depth int) (plistValue, error) {
	switch start.Name.Local {
	case "dict":
		entries := make(map[string]plistValue)
		for {
			key, found, err := decodeSchoolPlistKey(decoder)
			if err != nil {
				return plistValue{}, err
			}
			if !found {
				return plistValue{kind: plistDict, entries: entries}, nil
			}
			value, found, err := decodeSchoolPlistValue(decoder, depth+1)
			if err != nil {
				return plistValue{}, err
			}
			if !found {
				return plistValue{}, errSchoolPlistMalformed
			}
			entries[key] = value
		}
	case "array":
		var items []plistValue
		for {
			value, found, err := decodeSchoolPlistValue(decoder, depth+1)
			if err != nil {
				return plistValue{}, err
			}
			if !found {
				return plistValue{kind: plistArray, items: items}, nil
			}
			items = append(items, value)
		}
	case "key":
		// A <key> only ever appears as a dict entry name, and decodeSchoolPlistKey
		// consumes those.
		return plistValue{}, errSchoolPlistMalformed
	default:
		text, err := decodeSchoolPlistText(decoder, start)
		if err != nil {
			return plistValue{}, err
		}
		kind := plistScalar
		if start.Name.Local == "string" {
			kind = plistString
		}
		return plistValue{kind: kind, text: text}, nil
	}
}

func decodeSchoolPlistKey(decoder *xml.Decoder) (string, bool, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", false, errSchoolPlistMalformed
		}
		switch token := token.(type) {
		case xml.EndElement:
			return "", false, nil
		case xml.StartElement:
			if token.Name.Local != "key" {
				return "", false, errSchoolPlistMalformed
			}
			key, err := decodeSchoolPlistText(decoder, token)
			if err != nil {
				return "", false, err
			}
			return key, true, nil
		}
	}
}

func decodeSchoolPlistText(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", errSchoolPlistMalformed
		}
		switch token := token.(type) {
		case xml.CharData:
			text.Write(token)
		case xml.StartElement:
			return "", errSchoolPlistMalformed
		case xml.EndElement:
			if token.Name != start.Name {
				return "", errSchoolPlistMalformed
			}
			return text.String(), nil
		}
	}
}

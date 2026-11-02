package mdm

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerateMDMProfile(t *testing.T) {
	input := ProfileInput{
		BaseURL:        "https://example.jamfcloud.com",
		InvitationCode: `invite<&>"`,
	}

	profile, uuid, err := GenerateProfile(input, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if uuid != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("uuid=%q", uuid)
	}
	if err := ValidateProfile(profile, input, uuid); err != nil {
		t.Fatal(err)
	}

	text := string(profile)
	for _, want := range []string{
		"https://example.jamfcloud.com/enroll/profile",
		"invite&lt;&amp;&gt;&#34;",
		"<key>PayloadDescription</key><string>MDM Profile for mobile device management</string>",
		"<key>PayloadDisplayName</key><string>MDM Profile</string>",
		"<key>PayloadType</key><string>Profile Service</string>",
		"<key>PayloadIdentifier</key><string>397f22e4-4bd2-4533-a5ac-af41ff444b87</string>",
		"<key>PayloadVersion</key><integer>1</integer>",
		"<key>PayloadOrganization</key><string>JAMF Software</string>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("profile missing %q", want)
		}
	}

	last := -1
	for _, attribute := range []string{"UDID", "PRODUCT", "SERIAL", "VERSION", "DEVICE_NAME", "COMPROMISED"} {
		index := strings.Index(text, "<string>"+attribute+"</string>")
		if index <= last {
			t.Fatalf("device attribute %q is missing or out of order", attribute)
		}
		last = index
	}
}

func TestGenerateMDMProfileRejectsMissingInput(t *testing.T) {
	tests := []struct {
		name  string
		input ProfileInput
	}{
		{"missing base URL", ProfileInput{InvitationCode: "invite"}},
		{"missing invitation code", ProfileInput{BaseURL: "https://example.jamfcloud.com"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := GenerateProfile(tt.input, bytes.NewReader(make([]byte, 16))); err == nil {
				t.Fatal("GenerateProfile returned nil error")
			}
		})
	}
}

func TestGenerateMDMProfileNormalizesBaseURL(t *testing.T) {
	input := ProfileInput{
		BaseURL:        " https://example.jamfcloud.com/// ",
		InvitationCode: "invite",
	}
	profile, uuid, err := GenerateProfile(input, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(profile, []byte("https://example.jamfcloud.com/enroll/profile")) {
		t.Fatalf("profile does not contain normalized enrollment URL: %s", profile)
	}
	if bytes.Contains(profile, []byte("///")) {
		t.Fatalf("profile contains unnormalized URL: %s", profile)
	}
	if err := ValidateProfile(profile, input, uuid); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMDMProfileRejectsInvalidProfiles(t *testing.T) {
	input := ProfileInput{BaseURL: "https://example.jamfcloud.com", InvitationCode: "invite"}
	profile, uuid, err := GenerateProfile(input, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		profile []byte
	}{
		{"malformed XML", []byte("<plist>")},
		{"wrong URL", bytes.Replace(profile, []byte("https://example.jamfcloud.com/enroll/profile"), []byte("https://other.jamfcloud.com/enroll/profile"), 1)},
		{"wrong challenge", bytes.Replace(profile, []byte(">invite<"), []byte(">other<"), 1)},
		{"wrong UUID", bytes.Replace(profile, []byte(uuid), []byte("11111111-1111-4111-8111-111111111111"), 1)},
		{"wrong static value", bytes.Replace(profile, []byte(">Profile Service<"), []byte(">Other Service<"), 1)},
		{"wrong payload version", bytes.Replace(profile, []byte("<key>PayloadVersion</key><integer>1</integer>"), []byte("<key>PayloadVersion</key><integer>2</integer>"), 1)},
		{"wrong device attribute order", bytes.Replace(profile, []byte("<string>UDID</string>"), []byte("<string>PRODUCT</string>"), 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateProfile(tt.profile, input, uuid); err == nil {
				t.Fatal("ValidateProfile returned nil error")
			}
		})
	}
}

func TestValidateMDMProfileRejectsValuesOutsidePayloadContent(t *testing.T) {
	input := ProfileInput{BaseURL: "https://example.jamfcloud.com", InvitationCode: "invite"}
	profile, uuid, err := GenerateProfile(input, bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}

	url := []byte("<key>URL</key><string>https://example.jamfcloud.com/enroll/profile</string>")
	challenge := []byte("<key>Challenge</key><string>invite</string>")
	attributes := []byte("<key>DeviceAttributes</key><array>\n<string>UDID</string>\n<string>PRODUCT</string>\n<string>SERIAL</string>\n<string>VERSION</string>\n<string>DEVICE_NAME</string>\n<string>COMPROMISED</string>\n</array>")

	tests := []struct {
		name  string
		field []byte
	}{
		{"URL", url},
		{"challenge", challenge},
		{"device attributes", attributes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withoutField := bytes.Replace(profile, tt.field, nil, 1)
			moved := bytes.Replace(
				withoutField,
				[]byte("</dict>\n<key>PayloadDescription</key>"),
				append([]byte("</dict>\n"), append(tt.field, []byte("\n<key>PayloadDescription</key>")...)...),
				1,
			)
			if err := ValidateProfile(moved, input, uuid); err == nil {
				t.Fatal("ValidateProfile accepted values outside PayloadContent")
			}
		})
	}
}

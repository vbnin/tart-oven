package mdm

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	ProfileRemotePath  = "Desktop/mdm_enroll.mobileconfig"
	ProfileDisplayPath = "~/Desktop/mdm_enroll.mobileconfig"
	PayloadIdentifier  = "397f22e4-4bd2-4533-a5ac-af41ff444b87"
)

type ProfileInput struct {
	BaseURL        string
	InvitationCode string
}

const mdmProfileTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>PayloadContent</key><dict>
<key>URL</key><string>%s</string>
<key>Challenge</key><string>%s</string>
<key>DeviceAttributes</key><array>
<string>UDID</string>
<string>PRODUCT</string>
<string>SERIAL</string>
<string>VERSION</string>
<string>DEVICE_NAME</string>
<string>COMPROMISED</string>
</array></dict>
<key>PayloadDescription</key><string>MDM Profile for mobile device management</string>
<key>PayloadDisplayName</key><string>MDM Profile</string>
<key>PayloadType</key><string>Profile Service</string>
<key>PayloadIdentifier</key><string>397f22e4-4bd2-4533-a5ac-af41ff444b87</string>
<key>PayloadUUID</key><string>%s</string>
<key>PayloadVersion</key><integer>1</integer>
<key>PayloadOrganization</key><string>JAMF Software</string>
</dict></plist>
`

func GenerateProfile(input ProfileInput, random io.Reader) ([]byte, string, error) {
	baseURL, err := NormalizeJamfBaseURL(input.BaseURL)
	if err != nil {
		return nil, "", errors.New("invalid Jamf Pro base URL")
	}
	if baseURL == "" {
		return nil, "", errors.New("Jamf Pro base URL is required")
	}
	if input.InvitationCode == "" {
		return nil, "", errors.New("Jamf invitation code is required")
	}
	input.BaseURL = baseURL

	payloadUUID, err := newRandomUUID(random)
	if err != nil {
		return nil, "", fmt.Errorf("generate profile UUID: %w", err)
	}
	escURL, err := escapeMDMProfileText(input.BaseURL + "/enroll/profile")
	if err != nil {
		return nil, "", fmt.Errorf("escape profile URL: %w", err)
	}
	escCode, err := escapeMDMProfileText(input.InvitationCode)
	if err != nil {
		return nil, "", fmt.Errorf("escape invitation code: %w", err)
	}
	escUUID, err := escapeMDMProfileText(payloadUUID)
	if err != nil {
		return nil, "", fmt.Errorf("escape payload UUID: %w", err)
	}
	profile := []byte(fmt.Sprintf(
		mdmProfileTemplate,
		escURL,
		escCode,
		escUUID,
	))
	if err := ValidateProfile(profile, input, payloadUUID); err != nil {
		return nil, "", fmt.Errorf("validate generated profile: %w", err)
	}
	return profile, payloadUUID, nil
}

func newRandomUUID(random io.Reader) (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func escapeMDMProfileText(value string) (string, error) {
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		return "", fmt.Errorf("xml escape failed: %w", err)
	}
	return escaped.String(), nil
}

func ValidateProfile(profile []byte, input ProfileInput, payloadUUID string) error {
	baseURL, err := NormalizeJamfBaseURL(input.BaseURL)
	if err != nil {
		return errors.New("invalid Jamf Pro base URL")
	}
	if baseURL == "" || input.InvitationCode == "" || payloadUUID == "" {
		return errors.New("missing profile validation input")
	}
	input.BaseURL = baseURL
	rootValues, payloadContentValues, attributes, err := parseMDMProfile(profile)
	if err != nil {
		return err
	}
	payloadContentExpected := map[string]string{
		"URL":       input.BaseURL + "/enroll/profile",
		"Challenge": input.InvitationCode,
	}
	for key, want := range payloadContentExpected {
		if got, ok := payloadContentValues[key]; !ok || got != want {
			return fmt.Errorf("profile PayloadContent.%s does not match expected value", key)
		}
	}
	rootExpected := map[string]string{
		"PayloadDescription":  "MDM Profile for mobile device management",
		"PayloadDisplayName":  "MDM Profile",
		"PayloadType":         "Profile Service",
		"PayloadIdentifier":   PayloadIdentifier,
		"PayloadUUID":         payloadUUID,
		"PayloadVersion":      "1",
		"PayloadOrganization": "JAMF Software",
	}
	for key, want := range rootExpected {
		if got, ok := rootValues[key]; !ok || got != want {
			return fmt.Errorf("profile %s does not match expected value", key)
		}
	}

	expectedAttributes := []string{"UDID", "PRODUCT", "SERIAL", "VERSION", "DEVICE_NAME", "COMPROMISED"}
	if len(attributes) != len(expectedAttributes) {
		return errors.New("profile device attributes do not match")
	}
	for index, want := range expectedAttributes {
		if attributes[index] != want {
			return errors.New("profile device attributes do not match")
		}
	}
	return nil
}

type mdmProfileElement struct {
	name      xml.Name
	text      strings.Builder
	sourceKey string
	dict      *mdmProfileDict
}

type mdmProfileDict struct {
	root           bool
	payloadContent bool
}

func parseMDMProfile(profile []byte) (map[string]string, map[string]string, []string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(profile))
	rootValues := make(map[string]string)
	payloadContentValues := make(map[string]string)
	var attributes []string
	var stack []*mdmProfileElement
	var dicts []*mdmProfileDict
	pendingKey := ""
	deviceAttributesDepth := 0

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse profile XML: %w", err)
		}

		switch token := token.(type) {
		case xml.StartElement:
			element := &mdmProfileElement{name: token.Name}
			if token.Name.Local == "string" || token.Name.Local == "integer" {
				element.sourceKey = pendingKey
				if len(dicts) > 0 {
					element.dict = dicts[len(dicts)-1]
				}
				pendingKey = ""
			}
			stack = append(stack, element)
			if token.Name.Local == "array" {
				if pendingKey == "DeviceAttributes" && len(dicts) > 0 && dicts[len(dicts)-1].payloadContent {
					deviceAttributesDepth = len(stack)
				}
				pendingKey = ""
			} else if token.Name.Local == "dict" {
				dict := &mdmProfileDict{root: len(dicts) == 0}
				if len(dicts) == 1 && dicts[0].root && pendingKey == "PayloadContent" {
					dict.payloadContent = true
				}
				dicts = append(dicts, dict)
				pendingKey = ""
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write([]byte(token))
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != token.Name {
				return nil, nil, nil, errors.New("profile XML has mismatched elements")
			}
			element := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			switch element.name.Local {
			case "key":
				pendingKey = element.text.String()
			case "string", "integer":
				if deviceAttributesDepth > 0 && element.name.Local == "string" && len(stack) == deviceAttributesDepth {
					attributes = append(attributes, element.text.String())
				}
				if element.sourceKey != "" && element.dict != nil {
					values := rootValues
					if element.dict.payloadContent {
						values = payloadContentValues
					} else if !element.dict.root {
						continue
					}
					if _, exists := values[element.sourceKey]; exists {
						return nil, nil, nil, fmt.Errorf("profile contains duplicate %s", element.sourceKey)
					}
					values[element.sourceKey] = element.text.String()
				}
			case "array":
				if deviceAttributesDepth == len(stack)+1 {
					deviceAttributesDepth = 0
				}
			case "dict":
				if len(dicts) == 0 {
					return nil, nil, nil, errors.New("profile XML has mismatched dictionaries")
				}
				dicts = dicts[:len(dicts)-1]
			}
		}
	}
	if len(stack) != 0 || len(dicts) != 0 {
		return nil, nil, nil, errors.New("profile XML has unclosed elements")
	}
	return rootValues, payloadContentValues, attributes, nil
}

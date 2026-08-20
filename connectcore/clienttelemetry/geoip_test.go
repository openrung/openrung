package clienttelemetry

import "testing"

func TestDecodeGeoAttributes(t *testing.T) {
	body := []byte(`{"ip":"203.0.113.7","success":true,"country":"Japan","country_code":"JP","city":"Tokyo","connection":{"asn":2516,"org":"KDDI","isp":"au one net"}}`)
	attrs := decodeGeoAttributes(body)

	want := map[string]string{
		"client_ip":    "203.0.113.7",
		"country":      "Japan",
		"country_code": "JP",
		"city":         "Tokyo",
		"asn":          "AS2516",
		"isp":          "au one net",
		"organization": "KDDI",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Fatalf("attr %q = %q, want %q", k, attrs[k], v)
		}
	}
	// These are exactly the keys the broker dashboard reads for the geo columns.
	for _, key := range []string{"country", "country_code", "city", "isp"} {
		if attrs[key] == "" {
			t.Fatalf("dashboard geo key %q must be populated", key)
		}
	}
}

func TestDecodeGeoAttributesOmitsBlanks(t *testing.T) {
	body := []byte(`{"ip":"1.1.1.1","success":true,"country":"","country_code":"","city":"","connection":{}}`)
	attrs := decodeGeoAttributes(body)
	if _, ok := attrs["city"]; ok {
		t.Fatalf("blank city should be omitted: %+v", attrs)
	}
	if attrs["client_ip"] != "1.1.1.1" {
		t.Fatalf("client_ip = %q, want 1.1.1.1", attrs["client_ip"])
	}
}

func TestDecodeGeoAttributesFailure(t *testing.T) {
	if attrs := decodeGeoAttributes([]byte(`{"success":false}`)); attrs != nil {
		t.Fatalf("unsuccessful lookup should yield nil, got %+v", attrs)
	}
	if attrs := decodeGeoAttributes([]byte(`not json`)); attrs != nil {
		t.Fatalf("invalid JSON should yield nil, got %+v", attrs)
	}
}

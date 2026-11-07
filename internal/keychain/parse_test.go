package keychain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseKeychainValues(t *testing.T) {
	values, err := parse("/tmp/keychain.env", []byte("# comment\nexport REDDIT_ID='one'\nREDDIT_SECRET=two\nPLAIN=three\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, map[string]string{"REDDIT_ID": "one", "REDDIT_SECRET": "two", "PLAIN": "three"}) {
		t.Fatalf("values = %#v", values)
	}
	const secret = "never-print-this-value"
	_, err = parse("/tmp/keychain.env", []byte("BROKEN "+secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("parse error = %v, want redacted malformed-line error", err)
	}
}

func TestParseKeychainNamesDiscardsValues(t *testing.T) {
	const secret = "dashboard-must-never-see-this"
	names, err := parseNames("/tmp/keychain.env", []byte("# comment\nexport ALPHA="+secret+"\nBETA='another-secret'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]struct{}{"ALPHA": {}, "BETA": {}}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %#v, want %#v", names, want)
	}
	if strings.Contains(fmt.Sprint(names), secret) {
		t.Fatalf("names retained secret value: %#v", names)
	}
	if _, err := parseNames("/tmp/keychain.env", []byte("BROKEN "+secret)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed error = %v, want value-free error", err)
	}
}

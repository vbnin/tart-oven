package server

import (
	"reflect"
	"testing"
)

func TestCleanTagsKeepsOneSpellingAndDropsEmpties(t *testing.T) {
	got := cleanTags([]string{" macOS26 ", "MacOS26", "", "  ", "demo", "Demo", "two  words", "TWO WORDS"})
	want := []string{"macOS26", "demo", "two words"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if cleanTags(nil) != nil {
		t.Fatal("no tags should stay nil")
	}
}

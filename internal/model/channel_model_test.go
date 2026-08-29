package model

import (
	"reflect"
	"testing"
)

func TestNormalizeChannelModelList(t *testing.T) {
	got := NormalizeChannelModelList(" model-a, model-b,,model-a,  model-c ")
	if got != "model-a,model-b,model-c" {
		t.Fatalf("normalized models = %q", got)
	}
}

func TestChannelModelNamesCustomModelsTakePrecedence(t *testing.T) {
	got := ChannelModelNames("model-a,model-b,model-a", "model-b, model-c")
	want := []string{"model-a", "model-b", "model-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effective models = %#v, want %#v", got, want)
	}
}

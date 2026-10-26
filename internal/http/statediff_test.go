package httpserver

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiffStates(t *testing.T) {
	a := []byte(`{"serial":1,"outputs":{"pw":{"value":"old-out","sensitive":true},"gone":{"value":1}},"resources":[
		{"mode":"managed","type":"t","name":"x","instances":[{"attributes":{"id":"1","n":720,"tags":{"a":"b"},"admin_password":"hunter1","other":"s1"},
			"sensitive_attributes":[[{"type":"get_attr","value":"other"}]]}]},
		{"module":"module.m","mode":"data","type":"d","name":"y","instances":[{"index_key":"k","attributes":{"id":"2"}}]}]}`)
	b := []byte(`{"serial":2,"outputs":{"pw":{"value":"new-out","sensitive":true}},"resources":[
		{"mode":"managed","type":"t","name":"x","instances":[{"attributes":{"id":"1","n":0,"tags":{},"admin_password":"hunter2","other":"s2"},
			"sensitive_attributes":[[{"type":"get_attr","value":"other"}]]}]},
		{"mode":"managed","type":"t","name":"z","instances":[{"index_key":0,"attributes":{"id":"3","weird":"w"},"sensitive_attributes":"bad"}]}]}`)
	changes, err := diffStates(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(changes)
	for _, want := range []string{
		`{module.m.data.d.y["k"] delete [{id "2" }]}`,
		`{output.gone delete [{value 1 }]}`,
		`{output.pw update [{value (sensitive) (sensitive)}]}`,
		`{t.x update [{admin_password (sensitive) (sensitive)} {n 720 0} {other (sensitive) (sensitive)} {tags  {}} {tags.a "b" }]}`,
		`{t.z[0] create [{id  (sensitive)} {weird  (sensitive)}]}`, // unreadable sensitive_attributes: all hidden
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in\n%s", want, got)
		}
	}
	if strings.Contains(got, "hunter") || strings.Contains(got, "-out") || strings.Contains(got, "s1") {
		t.Fatalf("diff leaks a sensitive value: %s", got)
	}
	if c, _ := diffStates(a, a); len(c) != 0 {
		t.Fatalf("same state: %v", c)
	}
}

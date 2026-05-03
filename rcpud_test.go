package main

import (
	"reflect"
	"testing"
)

func TestParseScript(t *testing.T) {
	script := []byte("dir=/home/scott\ncmd=rc\nuser=scott\n")
	expected := map[string]string{
		"dir":  "/home/scott",
		"cmd":  "rc",
		"user": "scott",
	}
	
	result := parseScript(script)
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Expected %v, got %v", expected, result)
	}
}

func TestParseScriptWithQuotes(t *testing.T) {
	script := []byte("dir='/home/scott space'\ncmd=\"rc -l\"\n")
	expected := map[string]string{
		"dir": "/home/scott space",
		"cmd": "rc -l",
	}
	
	result := parseScript(script)
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("Expected %v, got %v", expected, result)
	}
}

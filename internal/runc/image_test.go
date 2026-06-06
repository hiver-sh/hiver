package runc

import (
	"reflect"
	"testing"
)

func TestFindExposedTcpPorts(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want []int
	}{
		{
			name: "sorts and keeps tcp only",
			in:   map[string]any{"9000/tcp": struct{}{}, "8080/tcp": struct{}{}, "53/udp": struct{}{}},
			want: []int{8080, 9000},
		},
		{
			name: "malformed entries skipped",
			in:   map[string]any{"abc/tcp": struct{}{}, "8080": struct{}{}, "443/tcp": struct{}{}},
			want: []int{443},
		},
		{
			name: "empty is non-nil",
			in:   nil,
			want: []int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findExposedTcpPorts(tt.in)
			if got == nil {
				t.Fatal("findExposedTcpPorts() = nil, want non-nil slice")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("findExposedTcpPorts() = %v, want %v", got, tt.want)
			}
		})
	}
}

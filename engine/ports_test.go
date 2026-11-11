package mongodb

import (
	"reflect"
	"testing"

	"github.com/databasifylabs/engine-sdk/dtos"
)

func TestCapabilitiesListTheMongodbPorts(t *testing.T) {
	want := dtos.Ports{Client: 25060, Peers: []int{}}
	if got := (Engine{}).Capabilities().Ports; !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %+v", got)
	}
}

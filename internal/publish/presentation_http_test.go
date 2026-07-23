package publish

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

func TestHTTPPublisherPostsAndDeletesPresentation(t *testing.T) {
	want := presencev2.Presentation{
		APIVersion:    presencev2.APIVersion,
		Mode:          "slots",
		PixelCapacity: 5,
		Pixels: []presencev2.Pixel{
			{
				Pixel:      0,
				InstanceID: "instance-a",
				State:      instancepresence.StateWorking,
			},
		},
		ActiveCount:         1,
		VisibleCount:        1,
		OverflowCount:       0,
		OverflowInstanceIDs: []instancepresence.InstanceID{},
	}

	var methods []string
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/presence/presentation" {
				t.Errorf("path = %q", r.URL.Path)
			}

			methods = append(methods, r.Method)

			switch r.Method {
			case http.MethodPost:
				var got presencev2.Presentation
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode presentation: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("presentation = %#v, want %#v", got, want)
				}
				w.WriteHeader(http.StatusNoContent)

			case http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)

			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		},
	))
	defer server.Close()

	publisher, err := NewHTTPPublisher(
		server.URL,
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := publisher.PublishPresentation(
		context.Background(),
		want,
	); err != nil {
		t.Fatal(err)
	}

	if err := publisher.RemovePresentation(
		context.Background(),
	); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(
		methods,
		[]string{http.MethodPost, http.MethodDelete},
	) {
		t.Fatalf("methods = %#v", methods)
	}
}

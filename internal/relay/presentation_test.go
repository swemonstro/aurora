package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

func validPresentation() presencev2.Presentation {
	return presencev2.Presentation{
		APIVersion:    presencev2.APIVersion,
		Mode:          "slots",
		PixelCapacity: 5,
		Pixels: []presencev2.Pixel{
			{
				Pixel:      0,
				InstanceID: "instance-a",
				State:      instancepresence.StateWorking,
			},
			{
				Pixel:      1,
				InstanceID: "instance-b",
				State:      instancepresence.StateAttention,
			},
		},
		ActiveCount:         2,
		VisibleCount:        2,
		OverflowCount:       0,
		OverflowInstanceIDs: []instancepresence.InstanceID{},
	}
}

func TestPresentationRoundTripAndDelete(t *testing.T) {
	_, handler := newTestHandler(t)
	want := validPresentation()

	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	post := httptest.NewRequest(
		http.MethodPost,
		"/presence/presentation",
		bytes.NewReader(body),
	)
	post.Header.Set("Content-Type", "application/json")
	postResponse := httptest.NewRecorder()
	handler.ServeHTTP(postResponse, post)

	if postResponse.Code != http.StatusNoContent {
		t.Fatalf(
			"POST status = %d, body = %s",
			postResponse.Code,
			postResponse.Body.String(),
		)
	}

	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		getResponse,
		httptest.NewRequest(
			http.MethodGet,
			"/presence/presentation",
			nil,
		),
	)

	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d", getResponse.Code)
	}

	var got presencev2.Presentation
	if err := json.NewDecoder(getResponse.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("presentation = %#v, want %#v", got, want)
	}

	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		deleteResponse,
		httptest.NewRequest(
			http.MethodDelete,
			"/presence/presentation",
			nil,
		),
	)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d", deleteResponse.Code)
	}

	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		missingResponse,
		httptest.NewRequest(
			http.MethodGet,
			"/presence/presentation",
			nil,
		),
	)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf(
			"GET after DELETE status = %d",
			missingResponse.Code,
		)
	}
}

func TestPresentationStoreReturnsDetachedCopies(t *testing.T) {
	var store Store
	original := validPresentation()

	store.SetPresentation(original)
	original.Pixels[0].State = instancepresence.StateIdle

	first, ok := store.Presentation()
	if !ok {
		t.Fatal("presentation missing")
	}
	if first.Pixels[0].State != instancepresence.StateWorking {
		t.Fatal("caller mutation leaked into store")
	}

	first.Pixels[0].State = instancepresence.StateAttention
	second, _ := store.Presentation()
	if second.Pixels[0].State != instancepresence.StateWorking {
		t.Fatal("returned slice mutation leaked into store")
	}
}

func TestPresentationRejectsUnknownFields(t *testing.T) {
	_, handler := newTestHandler(t)

	request := httptest.NewRequest(
		http.MethodPost,
		"/presence/presentation",
		bytes.NewBufferString(`{
			"api_version":2,
			"mode":"slots",
			"pixel_capacity":5,
			"pixels":[],
			"active_count":0,
			"visible_count":0,
			"overflow_count":0,
			"overflow_instance_ids":[],
			"unexpected":true
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

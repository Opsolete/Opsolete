package httpserver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestInvalidSubmitRerendersForm(t *testing.T) {
	rec := post(newServer(t, "../../catalog", false, testStore(t)), "/templates/storage-account", url.Values{"name": {"NO"}, "location": {"westeurope"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `<p class="err" id="name-error">3-24 lowercase letters or digits, unique in Azure.</p>`) || // the label's help says what fits
		!strings.Contains(rec.Body.String(), `<a href="#name-error">Account name: `) || // the error summary links the field's error
		!strings.Contains(rec.Body.String(), `placeholder="azure-storage-account-prod"`) {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

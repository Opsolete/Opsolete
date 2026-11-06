package httpserver

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAccountTokens(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	sfx := strings.ToLower(newID()[:8])
	email, other := "acct-"+sfx+"@example.com", "acct-other-"+sfx+"@example.com"
	h := newServerAs(t, "../../catalog", false, store, devAuth(t, store, email, false, "grp-acct-"+sfx))
	get(h, "/account/profile") // the dev login records the user and its groups
	var me, them string
	store.DB.QueryRow(`SELECT id FROM users WHERE email = $1`, email).Scan(&me)
	them, err := store.UpsertUser(ctx, "", other, "user", nil)
	if err != nil || me == "" {
		t.Fatalf("users: %q %v", me, err)
	}
	t.Cleanup(func() { store.DB.Exec(`DELETE FROM api_tokens WHERE user_id::text IN ($1, $2)`, me, them) })
	token := func(user string) string {
		tok, err := store.CreateUserToken(ctx, user, "terraform login", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	valid := func(tok string) bool { _, _, err := store.TokenUser(ctx, tok); return err == nil }

	if body := get(h, "/account/profile").Body.String(); !strings.Contains(body, email) || !strings.Contains(body, "<code>grp-acct-"+sfx+"</code>") ||
		!strings.Contains(body, `<nav aria-label="Account settings">`) {
		t.Fatal("profile shows the email and groups in the account menu")
	}
	if rec := get(h, "/account/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account page: %d", rec.Code)
	}

	a, b, theirs := token(me), token(me), token(them)
	list, _ := store.UserTokens(ctx, me)
	if len(list) != 2 {
		t.Fatalf("own tokens: %+v", list)
	}
	theirList, _ := store.UserTokens(ctx, them)
	if body := get(h, "/account/tokens").Body.String(); !strings.Contains(body, "/account/tokens/"+list[0].ID+"/revoke") ||
		strings.Contains(body, theirList[0].ID) {
		t.Fatal("the tokens page lists only the user's own tokens")
	}
	if rec := post(h, "/account/tokens/"+theirList[0].ID+"/revoke", nil); rec.Code != http.StatusSeeOther || !valid(theirs) {
		t.Fatal("another user's token can't be revoked")
	}
	post(h, "/account/tokens/"+list[0].ID+"/revoke", nil)
	if left, _ := store.UserTokens(ctx, me); len(left) != 1 || left[0].ID == list[0].ID {
		t.Fatalf("one token revoked: %+v", left)
	}
	if rec := post(h, "/account/tokens/revoke", nil); rec.Code != http.StatusSeeOther || valid(a) || valid(b) || !valid(theirs) {
		t.Fatal("revoke all: the user's tokens go, other users' stay")
	}
}

package admin

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDiffMembers(t *testing.T) {
	v := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	removed, added := diffMembers([]uuid.UUID{v[0], v[1]}, []uuid.UUID{v[1], v[2]})
	if len(removed) != 1 || removed[0] != v[0] || len(added) != 1 || added[0] != v[2] {
		t.Fatalf("removed=%v added=%v", removed, added)
	}
	removed, added = diffMembers(v, v)
	if len(removed) != 0 || len(added) != 0 {
		t.Fatalf("unchanged produced removed=%v added=%v", removed, added)
	}
}

func TestGrantChange(t *testing.T) {
	d1, d2 := uuid.New(), uuid.New()
	g := func(id uuid.UUID, a string) Grant { return Grant{DatabaseID: id, Access: a} }
	cases := []struct {
		name      string
		old, next []Grant
		dec, inc  bool
	}{
		{"none to view", nil, []Grant{g(d1, "view")}, false, true},
		{"view to edit", []Grant{g(d1, "view")}, []Grant{g(d1, "edit")}, false, true},
		{"edit to view", []Grant{g(d1, "edit")}, []Grant{g(d1, "view")}, true, false},
		{"removed", []Grant{g(d1, "view")}, nil, true, false},
		{"unchanged", []Grant{g(d1, "view")}, []Grant{g(d1, "view")}, false, false},
		{"mixed", []Grant{g(d1, "edit")}, []Grant{g(d1, "view"), g(d2, "view")}, true, true},
	}
	for _, c := range cases {
		dec, inc := grantChange(c.old, c.next)
		if dec != c.dec || inc != c.inc {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", c.name, dec, inc, c.dec, c.inc)
		}
	}
}

func TestSessionRotationOnAccessChangeLive(t *testing.T) {
	url := os.Getenv("ADMIN_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ADMIN_TEST_DATABASE_URL is not set")
	}
	ownerURL := os.Getenv("AUTH_TEST_CLEANUP_DATABASE_URL")
	if ownerURL == "" {
		t.Skip("AUTH_TEST_CLEANUP_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)

	var userIDs, groupIDs, connIDs []uuid.UUID
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 30*time.Second)
		defer cc()
		defer db.Close()
		defer owner.Close()
		for _, q := range []struct {
			sql string
			ids []uuid.UUID
		}{
			{`DELETE FROM app.session_families WHERE user_id = ANY($1)`, userIDs},
			{`DELETE FROM app.group_memberships WHERE user_id = ANY($1)`, userIDs},
			{`DELETE FROM app.group_database_grants WHERE group_id = ANY($1)`, groupIDs},
			{`DELETE FROM app.groups WHERE id = ANY($1)`, groupIDs},
			{`DELETE FROM app.database_connections WHERE id = ANY($1)`, connIDs},
			{`DELETE FROM app.users WHERE id = ANY($1)`, userIDs},
		} {
			if _, err := owner.Exec(c, q.sql, q.ids); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	mkUser := func() uuid.UUID {
		u, err := repo.CreateUser(ctx, "rot-"+uuid.NewString(), "TestPassword123!", false)
		if err != nil {
			t.Fatal(err)
		}
		userIDs = append(userIDs, u.ID)
		return u.ID
	}
	mkSession := func(u uuid.UUID) uuid.UUID {
		id := uuid.New()
		now := time.Now().UTC()
		if _, err := db.Exec(ctx, `INSERT INTO app.session_families(id,user_id,auth_version,created_at,last_seen_at,absolute_expires_at,rotated_at)
			VALUES($1,$2,1,$3,$3,$4,$3)`, id, u, now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mkConn := func() uuid.UUID {
		id := uuid.New()
		if _, err := owner.Exec(ctx, `INSERT INTO app.database_connections(id,name,host,port,database_name,username,password_ciphertext,credential_key_id,ssl_mode,target_key)
			VALUES($1,$2,'h',5432,'d','u','\x01','k','disable',$2)`, id, "rot-"+id.String()); err != nil {
			t.Fatal(err)
		}
		connIDs = append(connIDs, id)
		return id
	}
	state := func(f uuid.UUID) (revoked, required bool) {
		if err := db.QueryRow(ctx, `SELECT revoked_at IS NOT NULL, rotation_required FROM app.session_families WHERE id=$1`, f).Scan(&revoked, &required); err != nil {
			t.Fatal(err)
		}
		return
	}
	reset := func(fs ...uuid.UUID) {
		if _, err := db.Exec(ctx, `UPDATE app.session_families SET rotation_required=false WHERE id = ANY($1)`, fs); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(label string, f uuid.UUID, wantRevoked, wantRequired bool) {
		t.Helper()
		if r, q := state(f); r != wantRevoked || q != wantRequired {
			t.Errorf("%s: revoked=%v required=%v, want %v/%v", label, r, q, wantRevoked, wantRequired)
		}
	}

	a, b, c := mkUser(), mkUser(), mkUser()
	fa, fb, fc := mkSession(a), mkSession(b), mkSession(c)
	g, err := repo.CreateGroup(ctx, "rot-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	groupIDs = append(groupIDs, g.ID)

	if _, err := repo.ReplaceMembers(ctx, g.ID, []uuid.UUID{a, b}); err != nil {
		t.Fatal(err)
	}
	reset(fa, fb, fc)
	if _, err := repo.ReplaceMembers(ctx, g.ID, []uuid.UUID{b, c}); err != nil {
		t.Fatal(err)
	}
	expect("removed member", fa, true, false)
	expect("unchanged member", fb, false, false)
	expect("added member", fc, false, true)

	d := mkConn()
	reset(fb, fc)
	if _, err := repo.ReplaceGrants(ctx, g.ID, []Grant{{d, "view"}}); err != nil {
		t.Fatal(err)
	}
	expect("grant added b", fb, false, true)
	expect("grant added c", fc, false, true)
	reset(fb, fc)
	if _, err := repo.ReplaceGrants(ctx, g.ID, []Grant{{d, "view"}}); err != nil {
		t.Fatal(err)
	}
	expect("grant unchanged", fb, false, false)
	if _, err := repo.ReplaceGrants(ctx, g.ID, []Grant{{d, "edit"}}); err != nil {
		t.Fatal(err)
	}
	expect("view->edit b", fb, false, true)
	expect("view->edit c", fc, false, true)
	reset(fb, fc)
	if _, err := repo.ReplaceGrants(ctx, g.ID, []Grant{{d, "view"}}); err != nil {
		t.Fatal(err)
	}
	expect("edit->view b", fb, true, false)
	expect("edit->view c", fc, true, false)

	// DeleteGroup with grants revokes members.
	a2, b2 := mkUser(), mkUser()
	fa2, fb2 := mkSession(a2), mkSession(b2)
	g2, err := repo.CreateGroup(ctx, "rot-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	groupIDs = append(groupIDs, g2.ID)
	if _, err := repo.ReplaceMembers(ctx, g2.ID, []uuid.UUID{a2, b2}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReplaceGrants(ctx, g2.ID, []Grant{{d, "view"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteGroup(ctx, g2.ID); err != nil {
		t.Fatal(err)
	}
	if r, _ := state(fa2); !r {
		t.Error("DeleteGroup with grants: a2 not revoked")
	}
	if r, _ := state(fb2); !r {
		t.Error("DeleteGroup with grants: b2 not revoked")
	}
}

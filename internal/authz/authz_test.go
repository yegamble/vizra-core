package authz

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
)

const (
	ownerID   = "11111111-1111-7111-8111-111111111111"
	otherID   = "22222222-2222-7222-8222-222222222222"
	staffID   = "33333333-3333-7333-8333-333333333333"
	granteeID = "44444444-4444-7444-8444-444444444444"
)

// subjectFor builds the five viewer classes of ADR-007.
func subjectFor(class string) Subject {
	switch class {
	case "A":
		return Subject{Role: RoleAnonymous}
	case "M":
		return Subject{UserID: otherID, Role: RoleMember}
	case "G":
		return Subject{UserID: granteeID, Role: RoleMember, HasGrant: true, GrantAllowsDownload: true}
	case "O":
		return Subject{UserID: ownerID, Role: RoleMember}
	case "S":
		return Subject{UserID: staffID, Role: RoleAdmin, Staff: true}
	}
	panic("unknown viewer class " + class)
}

// resourceFor builds the base resource for a fixture row. Row 4 is special: the
// ADR says the album page is governed by album privacy, not item visibility, so
// its three columns drive album privacy.
func resourceFor(action Action, vis string) Resource {
	r := Resource{
		OwnerID:           ownerID,
		Visibility:        Visibility(vis),
		DownloadSetting:   DownloadAll,
		Scope:             ScopeSite,
		OwnerAllowsIPFS:   true,
		VisibilityVersion: 1,
	}
	if action == ActionAlbumPage {
		switch vis {
		case "public":
			r.AlbumPrivacy = AlbumPublic
		case "unlisted":
			r.AlbumPrivacy = AlbumLink
		case "private":
			r.AlbumPrivacy = AlbumPrivate
		}
	}
	return r
}

type fixtureRow struct {
	surface    Action
	visibility string
	expect     map[string]bool // viewer class -> allowed
	line       int
}

func loadFixture(t *testing.T) []fixtureRow {
	t.Helper()
	f, err := os.Open("testdata/adr007_matrix.tsv")
	if err != nil {
		t.Fatalf("the frozen matrix fixture is missing: %v", err)
	}
	defer f.Close()

	classes := []string{"A", "M", "G", "O", "S"}
	var rows []fixtureRow
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			t.Fatalf("testdata/adr007_matrix.tsv:%d: want 7 tab-separated fields, got %d: %q", n, len(fields), line)
		}
		row := fixtureRow{
			surface:    Action(strings.TrimSpace(fields[0])),
			visibility: strings.TrimSpace(fields[1]),
			expect:     map[string]bool{},
			line:       n,
		}
		for i, c := range classes {
			switch strings.TrimSpace(fields[2+i]) {
			case "allow":
				row.expect[c] = true
			case "deny":
				row.expect[c] = false
			default:
				t.Fatalf("testdata/adr007_matrix.tsv:%d: column %s must be allow or deny, got %q", n, c, fields[2+i])
			}
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return rows
}

// TestFrozenMatrix is the ADR-007 M0 obligation: the frozen surface x visibility
// matrix is the fixture of a table-driven test in internal/authz, and the
// evaluator exists before any product route.
func TestFrozenMatrix(t *testing.T) {
	e := NewEvaluator(Options{})
	ctx := context.Background()
	rows := loadFixture(t)

	const wantCases = 21 * 3 * 5
	if got := len(rows) * 5; got != wantCases {
		t.Fatalf("fixture covers %d cases, want %d (21 surfaces x 3 visibilities x 5 viewer classes); rows=%d",
			got, wantCases, len(rows))
	}

	for _, row := range rows {
		for _, class := range []string{"A", "M", "G", "O", "S"} {
			t.Run(string(row.surface)+"/"+row.visibility+"/"+class, func(t *testing.T) {
				got, reason := e.Decide(ctx, subjectFor(class), row.surface, resourceFor(row.surface, row.visibility))
				want := row.expect[class]
				if got.Allowed() != want {
					t.Fatalf("ADR-007 matrix (testdata/adr007_matrix.tsv:%d) says %s for %s/%s/%s; Decide returned %s (%s)",
						row.line, allowWord(want), row.surface, row.visibility, class, got, reason)
				}
			})
		}
	}
}

func allowWord(b bool) string {
	if b {
		return "allow"
	}
	return "deny"
}

// The fixture and the Actions slice must describe the same 21 surfaces.
func TestFixtureCoversEveryFrozenSurfaceExactlyOnce(t *testing.T) {
	rows := loadFixture(t)
	seen := map[Action]map[string]bool{}
	for _, r := range rows {
		if seen[r.surface] == nil {
			seen[r.surface] = map[string]bool{}
		}
		if seen[r.surface][r.visibility] {
			t.Errorf("fixture line %d repeats %s/%s", r.line, r.surface, r.visibility)
		}
		seen[r.surface][r.visibility] = true
	}
	for _, a := range Actions {
		for _, v := range []string{"public", "unlisted", "private"} {
			if !seen[a][v] {
				t.Errorf("fixture has no row for %s/%s", a, v)
			}
		}
	}
	for a := range seen {
		found := false
		for _, known := range Actions {
			if known == a {
				found = true
			}
		}
		if !found {
			t.Errorf("fixture names %q, which is not in authz.Actions", a)
		}
	}
	if len(Actions) != 21 {
		t.Errorf("authz.Actions has %d entries; the frozen matrix has 21 rows", len(Actions))
	}
	if len(kindOf) != len(Actions) {
		t.Errorf("kindOf covers %d actions, Actions has %d: an action with no kind denies silently", len(kindOf), len(Actions))
	}
}

// ADR-007 ruling 5: any surface absent from the matrix is DENY.
func TestUnlistedSurfaceDenies(t *testing.T) {
	e := NewEvaluator(Options{})
	// Even for the owner, on a public resource, with staff rights: an action
	// nobody wrote into the matrix must not fall open.
	for _, s := range []Subject{subjectFor("O"), subjectFor("S"), subjectFor("A")} {
		d, r := e.Decide(context.Background(), s, Action("a_surface_invented_in_M4"),
			Resource{OwnerID: ownerID, Visibility: VisibilityPublic})
		if d.Allowed() {
			t.Fatalf("an unlisted surface was allowed for %+v", s)
		}
		if r != ReasonSurfaceNotInMatrix {
			t.Fatalf("reason = %q, want %q", r, ReasonSurfaceNotInMatrix)
		}
	}
}

// Precedence step (1): on a private site, anonymous is denied every surface.
func TestSitePrivateDeniesAnonymousEverySurface(t *testing.T) {
	e := NewEvaluator(Options{SitePrivate: true})
	for _, a := range Actions {
		d, r := e.Decide(context.Background(), subjectFor("A"), a,
			Resource{OwnerID: ownerID, Visibility: VisibilityPublic, OwnerAllowsIPFS: true})
		if d.Allowed() {
			t.Errorf("private site allowed anonymous on %s", a)
		}
		if r != ReasonSitePrivate {
			t.Errorf("%s: reason = %q, want %q", a, r, ReasonSitePrivate)
		}
	}
	// A signed-in member is unaffected by site privacy mode.
	if d, _ := e.Decide(context.Background(), subjectFor("M"), ActionItemPage,
		Resource{OwnerID: ownerID, Visibility: VisibilityPublic}); !d.Allowed() {
		t.Error("private site denied a signed-in member a public item page")
	}
}

// Row 9 condition: the owner finds their own unlisted and private items only in
// their own library scope, and a grant never extends to search.
func TestSearchOwnLibraryScope(t *testing.T) {
	e := NewEvaluator(Options{})
	for _, vis := range []Visibility{VisibilityUnlisted, VisibilityPrivate} {
		r := Resource{OwnerID: ownerID, Visibility: vis, Scope: ScopeOwnLibrary}
		if d, _ := e.Decide(context.Background(), subjectFor("O"), ActionSearch, r); !d.Allowed() {
			t.Errorf("owner could not find own %s item in own-library scope", vis)
		}
		// Grants do not extend to search, in any scope.
		if d, _ := e.Decide(context.Background(), subjectFor("G"), ActionSearch, r); d.Allowed() {
			t.Errorf("a grant extended to search for a %s item", vis)
		}
		// Site scope still excludes it, even for the owner.
		r.Scope = ScopeSite
		if d, _ := e.Decide(context.Background(), subjectFor("O"), ActionSearch, r); d.Allowed() {
			t.Errorf("owner's %s item leaked into site-scope search", vis)
		}
	}
}

// Row 2 condition: the owner's download setting narrows, never widens.
func TestOriginalDownloadSetting(t *testing.T) {
	e := NewEvaluator(Options{})
	cases := []struct {
		setting DownloadSetting
		class   string
		want    bool
	}{
		{DownloadAll, "A", true}, {DownloadAll, "M", true},
		{DownloadMembers, "A", false}, {DownloadMembers, "M", true},
		{DownloadNobody, "A", false}, {DownloadNobody, "M", false},
		// The owner and audited staff are never narrowed by the setting.
		{DownloadNobody, "O", true}, {DownloadNobody, "S", true},
	}
	for _, c := range cases {
		r := Resource{OwnerID: ownerID, Visibility: VisibilityPublic, DownloadSetting: c.setting}
		d, reason := e.Decide(context.Background(), subjectFor(c.class), ActionOriginalDownload, r)
		if d.Allowed() != c.want {
			t.Errorf("download setting %s, class %s: got %s (%s), want %s",
				c.setting, c.class, d, reason, allowWord(c.want))
		}
	}
}

// Row 2 private column: G is allowed only when download is in the grant.
func TestPrivateOriginalRequiresDownloadInGrant(t *testing.T) {
	e := NewEvaluator(Options{})
	r := Resource{OwnerID: ownerID, Visibility: VisibilityPrivate, DownloadSetting: DownloadAll}

	g := subjectFor("G")
	if d, _ := e.Decide(context.Background(), g, ActionOriginalDownload, r); !d.Allowed() {
		t.Error("a grant carrying download was refused a private original")
	}
	g.GrantAllowsDownload = false
	d, reason := e.Decide(context.Background(), g, ActionOriginalDownload, r)
	if d.Allowed() {
		t.Error("a grant without download got a private original")
	}
	if reason != ReasonGrantLacksDownload {
		t.Errorf("reason = %q, want %q", reason, ReasonGrantLacksDownload)
	}
}

// Row 18: the ADR-008 fence. A public item whose owner has not listed it is not
// IPFS-eligible.
func TestIPFSRequiresListedOwner(t *testing.T) {
	e := NewEvaluator(Options{})
	r := Resource{OwnerID: ownerID, Visibility: VisibilityPublic, OwnerAllowsIPFS: false}
	for _, class := range []string{"A", "M", "G", "O", "S"} {
		d, reason := e.Decide(context.Background(), subjectFor(class), ActionIPFSPublication, r)
		if d.Allowed() {
			t.Errorf("class %s: IPFS publication allowed for an owner who has not listed", class)
		}
		if reason != ReasonIPFSNotEligible {
			t.Errorf("class %s: reason = %q, want %q", class, reason, ReasonIPFSNotEligible)
		}
	}
}

// Row 19: site export is by OWNER role. An admin acting as staff is not enough.
func TestSiteExportRequiresOwnerRole(t *testing.T) {
	e := NewEvaluator(Options{})
	r := Resource{OwnerID: otherID, Visibility: VisibilityPublic}

	if d, _ := e.Decide(context.Background(), subjectFor("S"), ActionExport, r); d.Allowed() {
		t.Error("an admin exported another user's data")
	}
	siteOwner := Subject{UserID: staffID, Role: RoleOwner, Staff: true}
	if d, _ := e.Decide(context.Background(), siteOwner, ActionExport, r); !d.Allowed() {
		t.Error("the site owner was refused a site export")
	}
}

// Row 4 in full: album privacy is Chevereto's verbatim set.
func TestAlbumPrivacy(t *testing.T) {
	e := NewEvaluator(Options{})
	cases := []struct {
		privacy AlbumPrivacy
		class   string
		want    bool
	}{
		{AlbumPublic, "A", true}, {AlbumPublic, "M", true},
		{AlbumLink, "A", false}, {AlbumLink, "G", true},
		{AlbumPassword, "A", false}, {AlbumPassword, "G", true},
		{AlbumPrivate, "A", false}, {AlbumPrivate, "M", false},
		{AlbumPrivate, "G", true}, {AlbumPrivate, "O", true}, {AlbumPrivate, "S", true},
	}
	for _, c := range cases {
		r := Resource{OwnerID: ownerID, AlbumPrivacy: c.privacy, Visibility: VisibilityPublic}
		if d, reason := e.Decide(context.Background(), subjectFor(c.class), ActionAlbumPage, r); d.Allowed() != c.want {
			t.Errorf("album %s, class %s: got %s (%s), want %s", c.privacy, c.class, d, reason, allowWord(c.want))
		}
	}
}

// ADR-003: private resources return 404 to non-viewers, hiding existence.
func TestHideExistenceOnlyForPrivate(t *testing.T) {
	if !HideExistence(Resource{Visibility: VisibilityPrivate}) {
		t.Error("a private resource must hide its existence")
	}
	for _, v := range []Visibility{VisibilityPublic, VisibilityUnlisted} {
		if HideExistence(Resource{Visibility: v}) {
			t.Errorf("%s must not hide existence: the URL is the capability, and a 404 would be a lie", v)
		}
	}
}

// The zero Decision must be Deny, so a code path that forgets to assign denies.
func TestZeroDecisionIsDeny(t *testing.T) {
	var d Decision
	if d.Allowed() {
		t.Fatal("the zero Decision allows; it must deny")
	}
}

func TestRoleOrdering(t *testing.T) {
	if !RoleOwner.AtLeast(RoleAdmin) || !RoleAdmin.AtLeast(RoleManager) ||
		!RoleManager.AtLeast(RoleMember) || !RoleMember.AtLeast(RoleGuest) {
		t.Fatal("the role enum is not ordered owner > admin > manager > member > guest")
	}
	if RoleMember.AtLeast(RoleAdmin) {
		t.Fatal("member outranks admin")
	}
	// "moderator" is not a role label (ADR-003); it must not rank.
	if Role("moderator").AtLeast(RoleGuest) {
		t.Fatal(`"moderator" ranks; it is not a Vizra role`)
	}
}

// Holding a staff role is not enough: the context must be an audited admin
// context (matrix row 20 note).
func TestStaffRoleWithoutAuditedContextIsNotStaff(t *testing.T) {
	e := NewEvaluator(Options{})
	admin := Subject{UserID: staffID, Role: RoleAdmin, Staff: false}
	r := Resource{OwnerID: ownerID, Visibility: VisibilityPrivate}
	if d, _ := e.Decide(context.Background(), admin, ActionItemPage, r); d.Allowed() {
		t.Fatal("an admin outside an audited context read a private item")
	}
	if d, _ := e.Decide(context.Background(), admin, ActionAdminConsole, r); d.Allowed() {
		t.Fatal("an admin outside an audited context reached the admin console surface")
	}
}

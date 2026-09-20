// Package authz is the single authorization evaluator (ADR-003). Every read
// surface calls Decide: the API, derivative and original serving, counts,
// search hydration, feeds, embeds and exports. There is no second place a
// visibility question may be answered.
//
// Two properties are load-bearing and are tested rather than documented:
//
//   - **Default deny.** An Action not in the frozen matrix denies, with reason
//     ReasonSurfaceNotInMatrix. ADR-007 ruling 5. A new read surface that
//     forgets to add itself therefore fails closed, which is the opposite of
//     the failure mode that made Vidra fix the same privacy bug four times.
//   - **404, not 403, for private resources.** HideExistence reports when a
//     denial must be rendered as "does not exist"; leaking 403 would confirm
//     the row.
//
// The matrix this file implements is frozen in ADR-007 § "Frozen surface ×
// visibility matrix". It is transcribed into testdata/adr007_matrix.tsv, which
// is the fixture of the table-driven test — the fixture is the ADR, not a copy
// of the code below, so a change to this logic that is not also a decision
// turns the suite red.
package authz

import "context"

// Role is the ordered enum on users.role (ADR-003): owner > admin > manager >
// member > guest. "moderator" is not a role label. RoleAnonymous is not a
// stored role; it is the absence of one.
type Role string

const (
	RoleAnonymous Role = "anonymous"
	RoleGuest     Role = "guest"
	RoleMember    Role = "member"
	RoleManager   Role = "manager"
	RoleAdmin     Role = "admin"
	RoleOwner     Role = "owner"
)

var roleRank = map[Role]int{
	RoleAnonymous: 0, RoleGuest: 1, RoleMember: 2, RoleManager: 3, RoleAdmin: 4, RoleOwner: 5,
}

// AtLeast reports whether r is at or above other in the ordered enum.
func (r Role) AtLeast(other Role) bool { return roleRank[r] >= roleRank[other] }

// Visibility is an asset's own visibility (ADR-007).
type Visibility string

const (
	VisibilityPublic   Visibility = "public"
	VisibilityUnlisted Visibility = "unlisted"
	VisibilityPrivate  Visibility = "private"
)

// AlbumPrivacy is Chevereto's verbatim set (ADR-007).
type AlbumPrivacy string

const (
	AlbumPublic   AlbumPrivacy = "public"
	AlbumPrivate  AlbumPrivacy = "private"
	AlbumLink     AlbumPrivacy = "link"
	AlbumPassword AlbumPrivacy = "password"
)

// DownloadSetting is the owner's original-download setting (matrix row 2).
type DownloadSetting string

const (
	DownloadAll     DownloadSetting = "all"
	DownloadMembers DownloadSetting = "members"
	DownloadNobody  DownloadSetting = "nobody"
)

// Scope distinguishes a site-wide read from the owner's own library. It is the
// only thing that lets an owner find their own unlisted and private items in
// search (matrix row 9). Grants never extend to search.
type Scope string

const (
	ScopeSite       Scope = "site"
	ScopeOwnLibrary Scope = "own_library"
)

// Action is a read surface. The Action constants below are exactly the 21 rows
// of the frozen matrix; anything else denies.
type Action string

const (
	ActionItemPage           Action = "item_page"            // row 1
	ActionOriginalDownload   Action = "original_download"    // row 2
	ActionDerivativeURL      Action = "derivative_url"       // row 3
	ActionAlbumPage          Action = "album_page"           // row 4
	ActionCountContribution  Action = "count_contribution"   // row 5
	ActionOwnerLibrary       Action = "owner_library"        // row 6
	ActionProfileGrid        Action = "profile_grid"         // row 7
	ActionExplore            Action = "explore"              // row 8
	ActionSearch             Action = "search"               // row 9
	ActionTagCategoryListing Action = "tag_category_listing" // row 10
	ActionFeeds              Action = "feeds"                // row 11
	ActionNotifications      Action = "notifications"        // row 12
	ActionFavoriteRating     Action = "favorite_rating"      // row 13
	ActionEmbed              Action = "embed"                // row 14
	ActionSitemap            Action = "sitemap"              // row 15
	ActionBulkDownload       Action = "bulk_download"        // row 16
	ActionFederationOutbound Action = "federation_outbound"  // row 17
	ActionIPFSPublication    Action = "ipfs_publication"     // row 18
	ActionExport             Action = "export"               // row 19
	ActionAdminConsole       Action = "admin_console"        // row 20
	ActionSharedCache        Action = "shared_cache"         // row 21
)

// Actions is the complete frozen set, in matrix-row order. The fixture test
// asserts the file and this slice agree, so a row added to one and not the
// other is a red test rather than a silent gap.
var Actions = []Action{
	ActionItemPage, ActionOriginalDownload, ActionDerivativeURL, ActionAlbumPage,
	ActionCountContribution, ActionOwnerLibrary, ActionProfileGrid, ActionExplore,
	ActionSearch, ActionTagCategoryListing, ActionFeeds, ActionNotifications,
	ActionFavoriteRating, ActionEmbed, ActionSitemap, ActionBulkDownload,
	ActionFederationOutbound, ActionIPFSPublication, ActionExport,
	ActionAdminConsole, ActionSharedCache,
}

// Subject is who is asking.
type Subject struct {
	// UserID is empty for an anonymous viewer.
	UserID string
	Role   Role
	// Staff marks an admin or manager acting in an *audited* administration or
	// moderation context. Holding the role is not enough: viewing a private
	// original is an audited action (matrix row 20), so the context must say so.
	Staff bool
	// HasGrant is true when the viewer holds a valid grant on the item, or on an
	// album containing it (link, password, named member; audience in the full
	// profile). Grant validity — secret match, not expired, not revoked — is
	// established before Decide is called.
	HasGrant bool
	// GrantAllowsDownload marks a grant that carries the original-download
	// capability (matrix row 2, private column).
	GrantAllowsDownload bool
}

// Anonymous reports whether no identity was presented.
func (s Subject) Anonymous() bool { return s.UserID == "" }

// Resource is what is being read.
type Resource struct {
	// OwnerID is the owning user. Empty means unowned (a site-level resource).
	OwnerID string
	// Visibility is the asset's own visibility. For an album-scoped action it is
	// the visibility of the item being considered.
	Visibility Visibility
	// AlbumPrivacy governs ActionAlbumPage. Empty defaults to AlbumPublic.
	AlbumPrivacy AlbumPrivacy
	// DownloadSetting is the owner's setting for row 2. Empty defaults to DownloadAll.
	DownloadSetting DownloadSetting
	// Scope distinguishes a site read from the owner's own library (row 9).
	Scope Scope
	// OwnerAllowsIPFS is the ADR-008 fence: publication is eligible only with a
	// listed owner (row 18).
	OwnerAllowsIPFS bool
	// VisibilityVersion is carried in every cache and CDN key (ADR-007). Decide
	// does not read it; it is here so a caller cannot build a cache key without
	// having had the row in hand.
	VisibilityVersion int64
}

// Decision is the answer. The zero value is Deny, so a code path that forgets
// to assign one denies.
type Decision int

const (
	Deny  Decision = 0
	Allow Decision = 1
)

func (d Decision) Allowed() bool { return d == Allow }

func (d Decision) String() string {
	if d == Allow {
		return "allow"
	}
	return "deny"
}

// Reason explains the decision. It is safe to log: it names no identifier.
type Reason string

const (
	ReasonSurfaceNotInMatrix Reason = "surface_not_in_matrix"
	ReasonSitePrivate        Reason = "site_private_anonymous"
	ReasonPublic             Reason = "public"
	ReasonOwner              Reason = "owner"
	ReasonStaffAudited       Reason = "staff_audited_context"
	ReasonGrant              Reason = "grant"
	ReasonReachableByLink    Reason = "reachable_by_link"
	ReasonPrivate            Reason = "private_requires_owner_grant_or_staff"
	ReasonPublicOnlySurface  Reason = "surface_lists_public_items_only"
	ReasonDownloadDisabled   Reason = "owner_download_setting"
	ReasonGrantLacksDownload Reason = "grant_does_not_include_download"
	ReasonOwnerOnlySurface   Reason = "surface_is_owner_scoped"
	ReasonStaffOnlySurface   Reason = "surface_is_staff_only"
	ReasonSiteOwnerRequired  Reason = "site_export_requires_owner_role"
	ReasonIPFSNotEligible    Reason = "owner_has_not_listed_for_ipfs"
	ReasonNotSharedCacheable Reason = "only_public_derivatives_are_shared_cacheable"
	ReasonOwnLibraryScope    Reason = "owner_in_own_library_scope"
)

// Options configure an Evaluator for one site.
type Options struct {
	// SitePrivate is precedence step (1): on a private site an anonymous viewer
	// is denied every surface except sign-in, owner claim and public health —
	// none of which are read surfaces, so none of them reach Decide.
	SitePrivate bool
}

// Evaluator is the one evaluator. Construct it per site.
type Evaluator struct{ opts Options }

// NewEvaluator builds an evaluator for one site.
func NewEvaluator(opts Options) *Evaluator { return &Evaluator{opts: opts} }

// HideExistence reports whether a denial must be rendered as 404 rather than
// 403: private resources return 404 to non-viewers, hiding existence (ADR-003).
func HideExistence(r Resource) bool { return r.Visibility == VisibilityPrivate }

// Decide is the signature ADR-003 fixes. ctx is accepted so the evaluator can
// carry a deadline and audit correlation when decisions become audit events;
// it is deliberately not used to discover the subject, because a subject
// smuggled in through context is exactly how a read surface fails open.
func (e *Evaluator) Decide(ctx context.Context, subject Subject, action Action, resource Resource) (Decision, Reason) {
	_ = ctx

	// Precedence (1): site privacy mode.
	if e.opts.SitePrivate && subject.Anonymous() {
		return Deny, ReasonSitePrivate
	}

	k, known := kindOf[action]
	if !known {
		// ADR-007 ruling 5: any surface absent from the matrix is DENY.
		return Deny, ReasonSurfaceNotInMatrix
	}

	owner := resource.OwnerID != "" && subject.UserID == resource.OwnerID
	staff := subject.Staff && subject.Role.AtLeast(RoleManager)
	vis := resource.Visibility
	if vis == "" {
		vis = VisibilityPublic
	}

	switch k {
	case kindStaffOnly: // row 20
		if staff {
			return Allow, ReasonStaffAudited
		}
		return Deny, ReasonStaffOnlySurface

	case kindOwnerScoped: // row 6
		if owner {
			return Allow, ReasonOwner
		}
		if staff {
			return Allow, ReasonStaffAudited
		}
		return Deny, ReasonOwnerOnlySurface

	case kindExport: // row 19
		if owner {
			return Allow, ReasonOwner
		}
		// Site export is by owner role, audited. Admin is not enough.
		if subject.Staff && subject.Role == RoleOwner {
			return Allow, ReasonStaffAudited
		}
		return Deny, ReasonSiteOwnerRequired

	case kindAlbum: // row 4 — album privacy governs, not item visibility
		priv := resource.AlbumPrivacy
		if priv == "" {
			priv = AlbumPublic
		}
		if owner {
			return Allow, ReasonOwner
		}
		if staff {
			return Allow, ReasonStaffAudited
		}
		switch priv {
		case AlbumPublic:
			return Allow, ReasonPublic
		case AlbumLink, AlbumPassword, AlbumPrivate:
			// link/password are capabilities; an explicit grant covers a private
			// album. Album privacy never widens an item: the item filter is
			// applied separately, per item, by the caller.
			if subject.HasGrant {
				return Allow, ReasonGrant
			}
			return Deny, ReasonPrivate
		}
		return Deny, ReasonPrivate

	case kindCount: // row 5
		switch vis {
		case VisibilityPublic:
			return Allow, ReasonPublic
		default:
			// Never contributes to a count shown to an anonymous or plain member.
			if owner {
				return Allow, ReasonOwner
			}
			if staff {
				return Allow, ReasonStaffAudited
			}
			if subject.HasGrant {
				return Allow, ReasonGrant
			}
			return Deny, ReasonPrivate
		}

	case kindListing, kindListingIPFS: // rows 7, 8, 10, 11, 15, 17, 18, 21
		if vis != VisibilityPublic {
			return Deny, ReasonPublicOnlySurface
		}
		if k == kindListingIPFS && !resource.OwnerAllowsIPFS {
			return Deny, ReasonIPFSNotEligible
		}
		if action == ActionSharedCache && vis != VisibilityPublic {
			return Deny, ReasonNotSharedCacheable
		}
		return Allow, ReasonPublic

	case kindSearch: // row 9
		if vis == VisibilityPublic {
			return Allow, ReasonPublic
		}
		// Grants do not extend to search. Only the owner, and only in their own
		// library scope, finds their own unlisted and private items.
		if owner && resource.Scope == ScopeOwnLibrary {
			return Allow, ReasonOwnLibraryScope
		}
		return Deny, ReasonPublicOnlySurface

	case kindDownload: // rows 2 and 16
		if !e.downloadSettingPermits(subject, resource, owner, staff) {
			return Deny, ReasonDownloadDisabled
		}
		switch vis {
		case VisibilityPublic, VisibilityUnlisted:
			return Allow, reasonForReachable(vis)
		default:
			if owner {
				return Allow, ReasonOwner
			}
			if staff {
				return Allow, ReasonStaffAudited
			}
			if subject.HasGrant {
				if !subject.GrantAllowsDownload {
					return Deny, ReasonGrantLacksDownload
				}
				return Allow, ReasonGrant
			}
			return Deny, ReasonPrivate
		}

	case kindDirect: // rows 1, 3, 12
		switch vis {
		case VisibilityPublic, VisibilityUnlisted:
			return Allow, reasonForReachable(vis)
		default:
			if owner {
				return Allow, ReasonOwner
			}
			if staff {
				return Allow, ReasonStaffAudited
			}
			if subject.HasGrant {
				return Allow, ReasonGrant
			}
			return Deny, ReasonPrivate
		}

	case kindDirectNoPrivate: // row 14 — a private item has no embed at all
		switch vis {
		case VisibilityPublic, VisibilityUnlisted:
			return Allow, reasonForReachable(vis)
		default:
			return Deny, ReasonPrivate
		}

	case kindAggregate: // row 13 — aggregates of private items are O/S only
		switch vis {
		case VisibilityPublic, VisibilityUnlisted:
			return Allow, reasonForReachable(vis)
		default:
			if owner {
				return Allow, ReasonOwner
			}
			if staff {
				return Allow, ReasonStaffAudited
			}
			return Deny, ReasonPrivate
		}
	}

	return Deny, ReasonSurfaceNotInMatrix
}

func reasonForReachable(v Visibility) Reason {
	if v == VisibilityUnlisted {
		return ReasonReachableByLink
	}
	return ReasonPublic
}

// downloadSettingPermits applies the owner's download setting, which narrows
// rows 2 and 16 but never widens them. The owner and audited staff are never
// narrowed by it: it governs other people's access to the owner's originals.
func (e *Evaluator) downloadSettingPermits(s Subject, r Resource, owner, staff bool) bool {
	if owner || staff {
		return true
	}
	switch r.DownloadSetting {
	case "", DownloadAll:
		return true
	case DownloadMembers:
		return !s.Anonymous()
	case DownloadNobody:
		return false
	}
	return false
}

type surfaceKind int

const (
	kindDirect surfaceKind = iota
	kindDirectNoPrivate
	kindAggregate
	kindDownload
	kindListing
	kindListingIPFS
	kindSearch
	kindOwnerScoped
	kindStaffOnly
	kindExport
	kindAlbum
	kindCount
)

// kindOf maps each frozen surface to the precedence shape that governs it. An
// Action missing from this map denies with ReasonSurfaceNotInMatrix, which is
// what makes "a new read surface that forgets the matrix" fail closed.
var kindOf = map[Action]surfaceKind{
	ActionItemPage:           kindDirect,
	ActionOriginalDownload:   kindDownload,
	ActionDerivativeURL:      kindDirect,
	ActionAlbumPage:          kindAlbum,
	ActionCountContribution:  kindCount,
	ActionOwnerLibrary:       kindOwnerScoped,
	ActionProfileGrid:        kindListing,
	ActionExplore:            kindListing,
	ActionSearch:             kindSearch,
	ActionTagCategoryListing: kindListing,
	ActionFeeds:              kindListing,
	ActionNotifications:      kindDirect,
	ActionFavoriteRating:     kindAggregate,
	ActionEmbed:              kindDirectNoPrivate,
	ActionSitemap:            kindListing,
	ActionBulkDownload:       kindDownload,
	ActionFederationOutbound: kindListing,
	ActionIPFSPublication:    kindListingIPFS,
	ActionExport:             kindExport,
	ActionAdminConsole:       kindStaffOnly,
	ActionSharedCache:        kindListing,
}

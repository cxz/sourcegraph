package resolvers

import (
	"context"
	"database/sql"
	"sync"

	"github.com/graph-gophers/graphql-go"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/pkg/errors"
	"github.com/sourcegraph/go-diff/diff"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/cmd/repo-updater/repos"
	ee "github.com/sourcegraph/sourcegraph/enterprise/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/db"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/internal/vcs/git"
)

func marshalChangesetSpecRandID(id string) graphql.ID {
	return relay.MarshalID("ChangesetSpec", id)
}

func unmarshalChangesetSpecID(id graphql.ID) (changesetSpecRandID string, err error) {
	err = relay.UnmarshalSpec(id, &changesetSpecRandID)
	return
}

var _ graphqlbackend.ChangesetSpecResolver = &changesetSpecResolver{}

type changesetSpecResolver struct {
	store       *ee.Store
	httpFactory *httpcli.Factory

	changesetSpec *campaigns.ChangesetSpec

	mappingFetcher *changesetSpecConnectionMappingFetcher

	repo *types.Repo

	planOnce sync.Once
	plan     *ee.Plan
	planErr  error

	mappingOnce sync.Once
	mapping     *ee.RewirerMapping
	mappingErr  error
}

func NewChangesetSpecResolver(ctx context.Context, store *ee.Store, cf *httpcli.Factory, changesetSpec *campaigns.ChangesetSpec) (*changesetSpecResolver, error) {
	// 🚨 SECURITY: db.Repos.GetByIDs uses the authzFilter under the hood and
	// filters out repositories that the user doesn't have access to.
	// In case we don't find a repository, it might be because it's deleted
	// or because the user doesn't have access.
	rs, err := db.Repos.GetByIDs(ctx, changesetSpec.RepoID)
	if err != nil {
		return nil, err
	}
	var repo *types.Repo
	// Not found is ok, the resolver will disguise as a HiddenChangesetResolver.
	if len(rs) == 1 {
		repo = rs[0]
	}

	return &changesetSpecResolver{
		store:         store,
		httpFactory:   cf,
		repo:          repo,
		changesetSpec: changesetSpec,
	}, nil
}

func NewChangesetSpecResolverWithRepo(store *ee.Store, cf *httpcli.Factory, repo *types.Repo, changesetSpec *campaigns.ChangesetSpec) *changesetSpecResolver {
	return &changesetSpecResolver{
		store:         store,
		httpFactory:   cf,
		repo:          repo,
		changesetSpec: changesetSpec,
	}
}

func (r *changesetSpecResolver) WithRewirerMappingFetcher(mappingFetcher *changesetSpecConnectionMappingFetcher) *changesetSpecResolver {
	r.mappingFetcher = mappingFetcher
	return r
}

func (r *changesetSpecResolver) ID() graphql.ID {
	// 🚨 SECURITY: This needs to be the RandID! We can't expose the
	// sequential, guessable ID.
	return marshalChangesetSpecRandID(r.changesetSpec.RandID)
}

func (r *changesetSpecResolver) Type() campaigns.ChangesetSpecDescriptionType {
	return r.changesetSpec.Spec.Type()
}

func (r *changesetSpecResolver) Description(ctx context.Context) (graphqlbackend.ChangesetDescription, error) {
	descriptionResolver := &changesetDescriptionResolver{
		desc:         r.changesetSpec.Spec,
		repoResolver: graphqlbackend.NewRepositoryResolver(r.repo),
		diffStat:     r.changesetSpec.DiffStat(),
	}

	return descriptionResolver, nil
}

func (r *changesetSpecResolver) ExpiresAt() *graphqlbackend.DateTime {
	return &graphqlbackend.DateTime{Time: r.changesetSpec.ExpiresAt()}
}

func (r *changesetSpecResolver) Operations(ctx context.Context) ([]campaigns.ReconcilerOperation, error) {
	plan, err := r.computePlan(ctx)
	if err != nil {
		return nil, err
	}
	ops := plan.Ops.ExecutionOrder()
	return ops, nil
}

func (r *changesetSpecResolver) Delta(ctx context.Context) (graphqlbackend.ChangesetSpecDeltaResolver, error) {
	plan, err := r.computePlan(ctx)
	if err != nil {
		return nil, err
	}
	if plan.Delta == nil {
		return &changesetSpecDeltaResolver{}, nil
	}
	return &changesetSpecDeltaResolver{delta: *plan.Delta}, nil
}

func (r *changesetSpecResolver) computePlan(ctx context.Context) (*ee.Plan, error) {
	r.planOnce.Do(func() {
		mapping, err := r.computeMapping(ctx)
		if err != nil {
			r.planErr = err
			return
		}
		campaign, err := r.computeCampaign(ctx)
		if err != nil {
			r.planErr = err
			return
		}
		rewirer := ee.NewChangesetRewirer(ee.RewirerMappings{mapping}, campaign, r.store, repos.NewDBStore(r.store.DB(), sql.TxOptions{}))
		changesets, err := rewirer.Rewire(ctx)
		if err != nil {
			r.planErr = err
			return
		}
		if len(changesets) != 1 {
			r.planErr = errors.New("rewirer did not return changeset")
			return
		}
		changeset := changesets[0]

		var previousSpec *campaigns.ChangesetSpec
		if changeset.PreviousSpecID != 0 {
			previousSpec, err = r.store.GetChangesetSpecByID(ctx, changeset.PreviousSpecID)
		}

		r.plan, r.planErr = ee.DeterminePlan(previousSpec, r.changesetSpec, changeset)
	})
	return r.plan, r.planErr
}

func (r *changesetSpecResolver) computeCampaign(ctx context.Context) (*campaigns.Campaign, error) {
	svc := ee.NewService(r.store, r.httpFactory)
	campaignSpec, err := r.store.GetCampaignSpec(ctx, ee.GetCampaignSpecOpts{ID: r.changesetSpec.CampaignSpecID})
	if err != nil {
		// r.planErr = err
		return nil, err
	}
	campaign, _, err := svc.ReconcileCampaign(ctx, campaignSpec)
	if err != nil {
		// r.planErr = err
		return nil, err
	}
	return campaign, nil
}

func (r *changesetSpecResolver) computeMapping(ctx context.Context) (*ee.RewirerMapping, error) {
	r.mappingOnce.Do(func() {
		if r.mappingFetcher != nil {
			r.mapping, r.mappingErr = r.mappingFetcher.ForChangesetSpec(ctx, r.changesetSpec.ID)
			return
		}
		campaign, err := r.computeCampaign(ctx)
		if err != nil {
			r.mappingErr = err
			return
		}
		mappings, err := r.store.GetRewirerMappings(ctx, ee.GetRewirerMappingsOpts{CampaignSpecID: r.changesetSpec.CampaignSpecID, CampaignID: campaign.ID})
		if err != nil {
			r.mappingErr = err
			return
		}
		if err := mappings.Hydrate(ctx, r.store); err != nil {
			r.mappingErr = err
			return
		}
		for _, m := range mappings {
			if m.ChangesetSpecID == r.changesetSpec.ID {
				r.mapping = m
				return
			}
		}
		r.mappingErr = errors.New("mapping not found, this should not happen")
	})
	return r.mapping, r.mappingErr
}

func (r *changesetSpecResolver) Changeset(ctx context.Context) (graphqlbackend.ChangesetResolver, error) {
	mapping, err := r.computeMapping(ctx)
	if err != nil {
		return nil, err
	}
	if mapping.Changeset == nil {
		return nil, nil
	}
	return NewChangesetResolver(r.store, r.httpFactory, mapping.Changeset, r.repo), nil
}

func (r *changesetSpecResolver) repoAccessible() bool {
	// If the repository is not nil, it's accessible
	return r.repo != nil
}

func (r *changesetSpecResolver) ToHiddenChangesetSpec() (graphqlbackend.HiddenChangesetSpecResolver, bool) {
	if r.repoAccessible() {
		return nil, false
	}

	return r, true
}

func (r *changesetSpecResolver) ToVisibleChangesetSpec() (graphqlbackend.VisibleChangesetSpecResolver, bool) {
	if !r.repoAccessible() {
		return nil, false
	}

	return r, true
}

var _ graphqlbackend.ChangesetDescription = &changesetDescriptionResolver{}

// changesetDescriptionResolver implements both ChangesetDescription
// interfaces: ExistingChangesetReferenceResolver and
// GitBranchChangesetDescriptionResolver.
type changesetDescriptionResolver struct {
	repoResolver *graphqlbackend.RepositoryResolver
	desc         *campaigns.ChangesetSpecDescription
	diffStat     diff.Stat
}

func (r *changesetDescriptionResolver) ToExistingChangesetReference() (graphqlbackend.ExistingChangesetReferenceResolver, bool) {
	if r.desc.IsImportingExisting() {
		return r, true
	}
	return nil, false
}
func (r *changesetDescriptionResolver) ToGitBranchChangesetDescription() (graphqlbackend.GitBranchChangesetDescriptionResolver, bool) {
	if r.desc.IsBranch() {
		return r, true
	}
	return nil, false
}

func (r *changesetDescriptionResolver) BaseRepository() *graphqlbackend.RepositoryResolver {
	return r.repoResolver
}
func (r *changesetDescriptionResolver) ExternalID() string { return r.desc.ExternalID }
func (r *changesetDescriptionResolver) BaseRef() string    { return git.AbbreviateRef(r.desc.BaseRef) }
func (r *changesetDescriptionResolver) BaseRev() string    { return r.desc.BaseRev }
func (r *changesetDescriptionResolver) HeadRepository() *graphqlbackend.RepositoryResolver {
	return r.repoResolver
}
func (r *changesetDescriptionResolver) HeadRef() string { return git.AbbreviateRef(r.desc.HeadRef) }
func (r *changesetDescriptionResolver) Title() string   { return r.desc.Title }
func (r *changesetDescriptionResolver) Body() string    { return r.desc.Body }
func (r *changesetDescriptionResolver) Published() campaigns.PublishedValue {
	return r.desc.Published
}

func (r *changesetDescriptionResolver) DiffStat() *graphqlbackend.DiffStat {
	return graphqlbackend.NewDiffStat(r.diffStat)
}

func (r *changesetDescriptionResolver) Diff(ctx context.Context) (graphqlbackend.PreviewRepositoryComparisonResolver, error) {
	diff, err := r.desc.Diff()
	if err != nil {
		return nil, err
	}
	return graphqlbackend.NewPreviewRepositoryComparisonResolver(ctx, r.repoResolver, r.desc.BaseRev, diff)
}

func (r *changesetDescriptionResolver) Commits() []graphqlbackend.GitCommitDescriptionResolver {
	var resolvers []graphqlbackend.GitCommitDescriptionResolver
	for _, c := range r.desc.Commits {
		resolvers = append(resolvers, &gitCommitDescriptionResolver{
			message:     c.Message,
			diff:        c.Diff,
			authorName:  c.AuthorName,
			authorEmail: c.AuthorEmail,
		})
	}
	return resolvers
}

var _ graphqlbackend.GitCommitDescriptionResolver = &gitCommitDescriptionResolver{}

type gitCommitDescriptionResolver struct {
	message     string
	diff        string
	authorName  string
	authorEmail string
}

func (r *gitCommitDescriptionResolver) Author() *graphqlbackend.PersonResolver {
	return graphqlbackend.NewPersonResolver(
		r.authorName,
		r.authorEmail,
		// Try to find the corresponding Sourcegraph user.
		true,
	)
}
func (r *gitCommitDescriptionResolver) Message() string { return r.message }
func (r *gitCommitDescriptionResolver) Subject() string {
	return graphqlbackend.GitCommitSubject(r.message)
}
func (r *gitCommitDescriptionResolver) Body() *string {
	body := graphqlbackend.GitCommitBody(r.message)
	if body == "" {
		return nil
	}
	return &body
}
func (r *gitCommitDescriptionResolver) Diff() string { return r.diff }

type changesetSpecDeltaResolver struct {
	delta ee.ChangesetSpecDelta
}

var _ graphqlbackend.ChangesetSpecDeltaResolver = &changesetSpecDeltaResolver{}

func (c *changesetSpecDeltaResolver) TitleChanged() bool {
	return c.delta.TitleChanged
}
func (c *changesetSpecDeltaResolver) BodyChanged() bool {
	return c.delta.BodyChanged
}
func (c *changesetSpecDeltaResolver) Undraft() bool {
	return c.delta.Undraft
}
func (c *changesetSpecDeltaResolver) BaseRefChanged() bool {
	return c.delta.BaseRefChanged
}
func (c *changesetSpecDeltaResolver) DiffChanged() bool {
	return c.delta.DiffChanged
}
func (c *changesetSpecDeltaResolver) CommitMessageChanged() bool {
	return c.delta.CommitMessageChanged
}
func (c *changesetSpecDeltaResolver) AuthorNameChanged() bool {
	return c.delta.AuthorNameChanged
}
func (c *changesetSpecDeltaResolver) AuthorEmailChanged() bool {
	return c.delta.AuthorEmailChanged
}

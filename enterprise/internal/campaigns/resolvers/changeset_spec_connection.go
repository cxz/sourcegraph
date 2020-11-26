package resolvers

import (
	"context"
	"strconv"
	"sync"

	"github.com/pkg/errors"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend/graphqlutil"
	ee "github.com/sourcegraph/sourcegraph/enterprise/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/db"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

var _ graphqlbackend.ChangesetSpecConnectionResolver = &changesetSpecConnectionResolver{}

type changesetSpecConnectionResolver struct {
	store       *ee.Store
	httpFactory *httpcli.Factory

	opts         ee.ListChangesetSpecsOpts
	mappingsOpts ee.GetRewirerMappingsOpts

	// Cache results because they are used by multiple fields
	once           sync.Once
	changesetSpecs campaigns.ChangesetSpecs
	reposByID      map[api.RepoID]*types.Repo
	next           int64
	err            error
}

func (r *changesetSpecConnectionResolver) TotalCount(ctx context.Context) (int32, error) {
	count, err := r.store.CountChangesetSpecs(ctx, ee.CountChangesetSpecsOpts{
		CampaignSpecID: r.opts.CampaignSpecID,
	})
	if err != nil {
		return 0, err
	}
	return int32(count), nil
}

func (r *changesetSpecConnectionResolver) PageInfo(ctx context.Context) (*graphqlutil.PageInfo, error) {
	_, _, next, err := r.compute(ctx)
	if err != nil {
		return nil, err
	}

	if next != 0 {
		// We don't use the RandID for pagination, because we can't paginate database
		// entries based on the RandID.
		return graphqlutil.NextPageCursor(strconv.Itoa(int(next))), nil
	}

	return graphqlutil.HasNextPage(false), nil
}

func (r *changesetSpecConnectionResolver) Nodes(ctx context.Context) ([]graphqlbackend.ChangesetSpecResolver, error) {
	changesetSpecs, reposByID, _, err := r.compute(ctx)
	if err != nil {
		return nil, err
	}

	fetcher := &changesetSpecConnectionMappingFetcher{
		store: r.store,
		opts:  r.mappingsOpts,
	}

	resolvers := make([]graphqlbackend.ChangesetSpecResolver, 0, len(changesetSpecs))
	for _, c := range changesetSpecs {
		repo := reposByID[c.RepoID]
		// If it's not in reposByID the repository was filtered out by the
		// authz-filter.
		// In that case we'll set it anyway to nil and changesetSpecResolver
		// will treat it as "hidden".

		resolvers = append(resolvers, NewChangesetSpecResolverWithRepo(r.store, r.httpFactory, repo, c).WithRewirerMappingFetcher(fetcher))
	}

	return resolvers, nil
}

func (r *changesetSpecConnectionResolver) compute(ctx context.Context) (campaigns.ChangesetSpecs, map[api.RepoID]*types.Repo, int64, error) {
	r.once.Do(func() {
		r.changesetSpecs, r.next, r.err = r.store.ListChangesetSpecs(ctx, r.opts)
		if r.err != nil {
			return
		}

		// 🚨 SECURITY: db.Repos.GetRepoIDsSet uses the authzFilter under the hood and
		// filters out repositories that the user doesn't have access to.
		r.reposByID, r.err = db.Repos.GetReposSetByIDs(ctx, r.changesetSpecs.RepoIDs()...)
	})

	return r.changesetSpecs, r.reposByID, r.next, r.err
}

type changesetSpecConnectionMappingFetcher struct {
	store *ee.Store
	opts  ee.GetRewirerMappingsOpts

	once        sync.Once
	mappingByID map[int64]*ee.RewirerMapping
	err         error
}

func (c *changesetSpecConnectionMappingFetcher) ForChangesetSpec(ctx context.Context, id int64) (*ee.RewirerMapping, error) {
	mappingByID, err := c.compute(ctx)
	if err != nil {
		return nil, err
	}
	mapping, ok := mappingByID[id]
	if !ok {
		return nil, errors.New("couldn't find mapping for changeset")
	}

	return mapping, nil
}

func (c *changesetSpecConnectionMappingFetcher) compute(ctx context.Context) (map[int64]*ee.RewirerMapping, error) {
	c.once.Do(func() {
		mappings, err := c.store.GetRewirerMappings(ctx, c.opts)
		if err != nil {
			c.err = err
			return
		}
		if err := mappings.Hydrate(ctx, c.store); err != nil {
			c.err = err
			return
		}

		c.mappingByID = make(map[int64]*ee.RewirerMapping)
		for _, m := range mappings {
			if m.ChangesetSpecID == 0 {
				continue
			}
			c.mappingByID[m.ChangesetSpecID] = m
		}
	})
	return c.mappingByID, c.err
}

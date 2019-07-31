package changesets

import (
	"context"
	"strconv"

	"github.com/graph-gophers/graphql-go"
	"github.com/pkg/errors"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/graphqlbackend"
	"github.com/sourcegraph/sourcegraph/enterprise/cmd/frontend/internal/comments"
	"github.com/sourcegraph/sourcegraph/enterprise/cmd/frontend/internal/threadlike"
	"github.com/sourcegraph/sourcegraph/enterprise/cmd/frontend/internal/threadlike/internal"
)

// 🚨 SECURITY: TODO!(sqs): there needs to be security checks everywhere here! there are none

// gqlChangeset implements the GraphQL type Changeset.
type gqlChangeset struct {
	threadlike.GQLThreadlike
	db *internal.DBThread
}

func newGQLChangeset(db *internal.DBThread) *gqlChangeset {
	return &gqlChangeset{
		GQLThreadlike: threadlike.GQLThreadlike{
			DB:             db,
			PartialComment: comments.GraphQLResolver{}.LazyCommentByID(threadlike.MarshalID(threadlike.GQLTypeChangeset, db.ID)),
		},
		db: db,
	}
}

// changesetByID looks up and returns the Changeset with the given GraphQL ID. If no such Changeset exists, it
// returns a non-nil error.
func changesetByID(ctx context.Context, id graphql.ID) (*gqlChangeset, error) {
	dbID, err := threadlike.UnmarshalIDOfType(threadlike.GQLTypeChangeset, id)
	if err != nil {
		return nil, err
	}
	return changesetByDBID(ctx, dbID)
}

func (GraphQLResolver) ChangesetByID(ctx context.Context, id graphql.ID) (graphqlbackend.Changeset, error) {
	return changesetByID(ctx, id)
}

// changesetByDBID looks up and returns the Changeset with the given database ID. If no such Changeset exists,
// it returns a non-nil error.
func changesetByDBID(ctx context.Context, dbID int64) (*gqlChangeset, error) {
	v, err := internal.DBThreads{}.GetByID(ctx, dbID)
	if err != nil {
		return nil, err
	}
	return newGQLChangeset(v), nil
}

func (v *gqlChangeset) ID() graphql.ID {
	return threadlike.MarshalID(threadlike.GQLTypeChangeset, v.db.ID)
}

func (GraphQLResolver) ChangesetInRepository(ctx context.Context, repositoryID graphql.ID, number string) (graphqlbackend.Changeset, error) {
	changesetDBID, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return nil, err
	}
	// TODO!(sqs): access checks
	changeset, err := changesetByDBID(ctx, changesetDBID)
	if err != nil {
		return nil, err
	}

	// TODO!(sqs): check that the changeset is indeed in the repo. When we make the changeset number
	// sequence per-repo, this will become necessary to even retrieve the changeset. for now, the ID is
	// global, so we need to perform this check.
	assertedRepo, err := graphqlbackend.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	if changeset.db.RepositoryID != assertedRepo.DBID() {
		return nil, errors.New("changeset does not exist in repository")
	}

	return changeset, nil
}

func (v *gqlChangeset) Status() graphqlbackend.ChangesetStatus {
	return graphqlbackend.ChangesetStatus(v.db.Status)
}

func (v *gqlChangeset) BaseRef() string { return v.db.BaseRef }

func (v *gqlChangeset) HeadRef() string { return v.db.HeadRef }

func (v *gqlChangeset) IsPreview() bool { return v.db.IsPreview }

func (v *gqlChangeset) RepositoryComparison(ctx context.Context) (*graphqlbackend.RepositoryComparisonResolver, error) {
	repo, err := v.Repository(ctx)
	if err != nil {
		return nil, err
	}
	return graphqlbackend.NewRepositoryComparison(ctx, repo, &graphqlbackend.RepositoryComparisonInput{
		Base: &v.db.BaseRef,
		Head: &v.db.HeadRef,
	})
}

package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestListAdaptiveScoreOptionsUsesCursorAndSoftDeleteProjection(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := NewAdminGroupRepository(client, db)

	mock.ExpectQuery(`SELECT "groups"."id", "groups"."name", "groups"."description" FROM "groups" WHERE "groups"."id" > $1 AND "groups"."deleted_at" IS NULL ORDER BY "groups"."id" ASC LIMIT 201`).
		WithArgs(int64(200)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description"}).
			AddRow(int64(201), "Next group", "Searchable").
			AddRow(int64(202), "No description", nil))

	options, err := repo.ListAdaptiveScoreOptions(context.Background(), 200, 201)
	require.NoError(t, err)
	require.Equal(t, []service.AdaptiveScoreOption{
		{ID: 201, Name: "Next group", Description: "Searchable"},
		{ID: 202, Name: "No description"},
	}, options)
	require.NoError(t, mock.ExpectationsWereMet())
}

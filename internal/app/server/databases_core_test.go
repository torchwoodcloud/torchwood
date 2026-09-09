package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/documents"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNewDatabases_HoldsSharedDocumentsCore(t *testing.T) {
	d := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)
	require.NotNil(t, d.docs)
}

func TestCreateDocument_GoesThroughSharedCore(t *testing.T) {
	catalog := collectionDocDB{fakeDocDB: newFakeDocDB()}
	rec := collectionDocDB{fakeDocDB: newFakeDocDB()}
	d := &Databases{
		projectRepo: fakeProjectRepo{},
		docDB:       catalog,
		docs:        documents.New(rec, nil),
	}
	principal := databases.Principal{Roles: []string{"keys"}}
	created, _, err := d.CreateDocument(context.Background(), "p1", "db1", "coll1", "d1", map[string]any{"a": 1}, nil, principal, "")
	require.NoError(t, err)
	require.Equal(t, "d1", created.ID)

	got, err := rec.GetDocument(context.Background(), "p1", "db1", "coll1", "d1", principal)
	require.NoError(t, err)
	require.NotNil(t, got)

	missing, err := catalog.GetDocument(context.Background(), "p1", "db1", "coll1", "d1", principal)
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestCreateDocument_KeysAllowPrivilegedGrant(t *testing.T) {
	d := NewDatabases(fakeProjectRepo{}, collectionDocDB{fakeDocDB: newFakeDocDB()}, nil)
	keys := databases.Principal{Roles: []string{"keys"}}
	_, _, err := d.CreateDocument(context.Background(), "p1", "db1", "coll1", "d1", map[string]any{"a": 1}, []databases.Permission{
		{Type: "update", Role: "user:other"},
	}, keys, "")
	require.NoError(t, err)

	user := databases.Principal{Roles: []string{"users", "user:u1"}}
	_, _, err = d.CreateDocument(context.Background(), "p1", "db1", "coll1", "d2", map[string]any{"a": 1}, []databases.Permission{
		{Type: "update", Role: "user:other"},
	}, user, "")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

package repository

import (
	"context"

	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The wire-admission check must not load account credentials on every request.
func (r *accountRepository) HasCPRInGroup(ctx context.Context, groupID int64) (bool, error) {
	return r.client.Account.Query().Where(
		dbaccount.DeletedAtIsNil(),
		dbaccount.TypeEQ(service.AccountTypeCPR),
		dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(groupID)),
	).Exist(ctx)
}

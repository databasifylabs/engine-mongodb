package mongodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/requests"
	"github.com/databasifylabs/engine-sdk/responses"
)

const (
	scramSha256           = "SCRAM-SHA-256"
	placeholderCollection = "_databasify"
	codeNamespaceExists   = 48
	codeUserNotFound      = 11
)

type accounts struct{ Engine }

func (e Engine) Accounts() contract.Accounts { return accounts{e} }

type databaseNames struct {
	Databases []struct {
		Name string `bson:"name"`
	} `bson:"databases"`
}

type userList struct {
	Users []struct {
		User  string `bson:"user"`
		Roles []struct {
			Role string `bson:"role"`
			Db   string `bson:"db"`
		} `bson:"roles"`
	} `bson:"users"`
}

func (e accounts) databases(ctx context.Context) ([]string, error) {
	var list databaseNames
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: true}}, &list); err != nil {
		return nil, err
	}
	names := []string{}
	for _, database := range list.Databases {
		if !slices.Contains(e.Capabilities().ReservedDatabases, database.Name) {
			names = append(names, database.Name)
		}
	}
	slices.Sort(names)

	return names, nil
}

func (e accounts) users(ctx context.Context) (userList, error) {
	var list userList

	return list, e.admin.Run(ctx, "admin", bson.D{{Key: "usersInfo", Value: 1}}, &list)
}

func (e accounts) List(ctx context.Context) (responses.AccountList, error) {
	databases, err := e.databases(ctx)
	if err != nil {
		return responses.AccountList{}, err
	}
	list, err := e.users(ctx)
	if err != nil {
		return responses.AccountList{}, err
	}
	users := []string{}
	grants := map[string][]dtos.Grant{}
	for _, user := range list.Users {
		if slices.Contains(e.Capabilities().ReservedUsers, user.User) {
			continue
		}
		users = append(users, user.User)
		for _, role := range user.Roles {
			access := map[string]enums.Access{"read": enums.AccessRead, "readWrite": enums.AccessWrite}[role.Role]
			if access != "" && slices.Contains(databases, role.Db) {
				grants[user.User] = append(grants[user.User], dtos.Grant{Database: role.Db, Access: access})
			}
		}
	}
	slices.Sort(users)

	return responses.AccountList{Users: users, Databases: databases, Grants: grants}, nil
}

func (e accounts) CreateDatabase(ctx context.Context, request requests.DatabaseCreate) (responses.DatabaseCreate, error) {
	if slices.Contains(e.Capabilities().ReservedDatabases, request.Name) {
		return responses.DatabaseCreate{}, command.Invalid(request.Name + " is reserved")
	}
	existing, err := e.databases(ctx)
	if err != nil {
		return responses.DatabaseCreate{}, err
	}
	err = e.admin.Run(ctx, request.Name, bson.D{{Key: "create", Value: placeholderCollection}}, nil)
	if err != nil && !hasCode(err, codeNamespaceExists) {
		return responses.DatabaseCreate{}, err
	}

	return responses.DatabaseCreate{Created: !slices.Contains(existing, request.Name)}, nil
}

func (e accounts) DropDatabase(ctx context.Context, request requests.DatabaseDrop) (responses.DatabaseDrop, error) {
	if slices.Contains(e.Capabilities().ReservedDatabases, request.Name) {
		return responses.DatabaseDrop{}, command.Invalid(request.Name + " is reserved")
	}
	existing, err := e.databases(ctx)
	if err != nil {
		return responses.DatabaseDrop{}, err
	}
	if err := e.revokeRolesOn(ctx, request.Name); err != nil {
		return responses.DatabaseDrop{}, err
	}

	return responses.DatabaseDrop{Dropped: slices.Contains(existing, request.Name)}, e.admin.Run(ctx, request.Name, bson.D{{Key: "dropDatabase", Value: 1}}, nil)
}

func (e accounts) revokeRolesOn(ctx context.Context, database string) error {
	list, err := e.users(ctx)
	if err != nil {
		return err
	}
	for _, user := range list.Users {
		revoked := bson.A{}
		for _, role := range user.Roles {
			if role.Db == database {
				revoked = append(revoked, bson.D{{Key: "role", Value: role.Role}, {Key: "db", Value: role.Db}})
			}
		}
		if len(revoked) == 0 {
			continue
		}
		if err := e.admin.Run(ctx, "admin", bson.D{{Key: "revokeRolesFromUser", Value: user.User}, {Key: "roles", Value: revoked}}, nil); err != nil {
			return err
		}
	}

	return nil
}

func (e accounts) encryptionOf(requested *string, password string) (string, error) {
	capabilities := e.Capabilities()
	encryption := *capabilities.DefaultPasswordEncryption
	if requested != nil {
		encryption = *requested
	}
	if !slices.Contains(capabilities.PasswordEncryptions, encryption) {
		return "", command.Invalid(fmt.Sprintf("password encryption %q is not offered", encryption))
	}
	if encryption == scramSha256 && strings.IndexFunc(password, unicode.IsControl) >= 0 {
		return "", command.Invalid("a SCRAM-SHA-256 password cannot contain control characters such as a newline or tab")
	}

	return encryption, nil
}

func (e accounts) rolesOf(ctx context.Context, grants []dtos.Grant, replication bool) (bson.A, error) {
	existing, err := e.databases(ctx)
	if err != nil {
		return nil, err
	}
	roles := bson.A{}
	for _, grant := range grants {
		if slices.Contains(e.Capabilities().ReservedDatabases, grant.Database) {
			return nil, command.Invalid(grant.Database + " is reserved")
		}
		if !slices.Contains(existing, grant.Database) {
			return nil, command.NotFound(fmt.Sprintf("database %q does not exist", grant.Database))
		}
		role := "read"
		if grant.Access == enums.AccessWrite {
			role = "readWrite"
		}
		roles = append(roles, bson.D{{Key: "role", Value: role}, {Key: "db", Value: grant.Database}})
	}
	if replication {
		roles = append(roles, bson.D{{Key: "role", Value: "clusterMonitor"}, {Key: "db", Value: "admin"}})
	}

	return roles, nil
}

func (e accounts) refuseReserved(name string) error {
	if slices.Contains(e.Capabilities().ReservedUsers, name) {
		return command.Invalid(name + " is reserved")
	}

	return nil
}

func (e accounts) CreateUser(ctx context.Context, request requests.UserCreate) (responses.UserCreate, error) {
	if err := e.refuseReserved(request.Name); err != nil {
		return responses.UserCreate{}, err
	}
	encryption, err := e.encryptionOf(request.Encryption, request.Password)
	if err != nil {
		return responses.UserCreate{}, err
	}
	roles, err := e.rolesOf(ctx, request.Grants, false)
	if err != nil {
		return responses.UserCreate{}, err
	}
	err = e.admin.Run(ctx, "admin", bson.D{{Key: "createUser", Value: request.Name}, {Key: "pwd", Value: request.Password}, {Key: "mechanisms", Value: bson.A{encryption}}, {Key: "roles", Value: roles}}, nil)
	if !alreadyExists(err) {
		return responses.UserCreate{Created: err == nil}, err
	}

	return responses.UserCreate{}, e.admin.Run(ctx, "admin", bson.D{{Key: "updateUser", Value: request.Name}, {Key: "pwd", Value: request.Password}, {Key: "mechanisms", Value: bson.A{encryption}}, {Key: "roles", Value: roles}}, nil)
}

func (e accounts) SetPassword(ctx context.Context, request requests.UserSetPassword) error {
	if err := e.refuseReserved(request.Name); err != nil {
		return err
	}
	encryption, err := e.encryptionOf(request.Encryption, request.Password)
	if err != nil {
		return err
	}

	return notFoundAsUser(request.Name, e.admin.Run(ctx, "admin", bson.D{{Key: "updateUser", Value: request.Name}, {Key: "pwd", Value: request.Password}, {Key: "mechanisms", Value: bson.A{encryption}}}, nil))
}

func (e accounts) Grant(ctx context.Context, request requests.UserGrant) error {
	if err := e.refuseReserved(request.Name); err != nil {
		return err
	}
	roles, err := e.rolesOf(ctx, request.Grants, request.Replication)
	if err != nil {
		return err
	}

	return notFoundAsUser(request.Name, e.admin.Run(ctx, "admin", bson.D{{Key: "updateUser", Value: request.Name}, {Key: "roles", Value: roles}}, nil))
}

func (e accounts) DropUser(ctx context.Context, request requests.UserDrop) (responses.UserDrop, error) {
	if err := e.refuseReserved(request.Name); err != nil {
		return responses.UserDrop{}, err
	}
	err := e.admin.Run(ctx, "admin", bson.D{{Key: "dropUser", Value: request.Name}}, nil)
	if hasCode(err, codeUserNotFound) {
		return responses.UserDrop{}, nil
	}

	return responses.UserDrop{Dropped: err == nil}, err
}

func notFoundAsUser(name string, err error) error {
	if hasCode(err, codeUserNotFound) {
		return command.NotFound(fmt.Sprintf("user %q does not exist", name))
	}

	return err
}

func hasCode(err error, code int) bool {
	var server mongo.ServerError

	return errors.As(err, &server) && server.HasErrorCode(code)
}

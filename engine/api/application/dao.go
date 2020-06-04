package application

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/go-gorp/gorp"

	"github.com/ovh/cds/engine/api/database/gorpmapping"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/log"
)

type dbApplication struct {
	gorpmapping.SignedEntity
	sdk.Application
}

func (e dbApplication) Canonical() gorpmapping.CanonicalForms {
	var _ = []interface{}{e.ProjectID, e.Name}
	return gorpmapping.CanonicalForms{
		"{{print .ProjectID}}{{.Name}}",
	}
}

func getAll(ctx context.Context, db gorp.SqlExecutor, query gorpmapping.Query, opts ...LoadOptionFunc) ([]sdk.Application, error) {
	var as []dbApplication
	if err := gorpmapping.GetAll(ctx, db, query, &as, gorpmapping.GetOptions.WithDecryption); err != nil {
		return nil, err
	}

	verifiedApplications := make([]*sdk.Application, 0, len(as))
	for i := range as {
		isValid, err := gorpmapping.CheckSignature(as[i], as[i].Signature)
		if err != nil {
			return nil, err
		}
		if !isValid {
			log.Error(ctx, "application.loadApplications> application %d data corrupted", as[i].ID)
			continue
		}
		verifiedApplications = append(verifiedApplications, &as[i].Application)
	}

	if len(verifiedApplications) > 0 {
		for i := range opts {
			if err := opts[i](ctx, db, verifiedApplications...); err != nil {
				return nil, err
			}
		}
	}

	apps := make([]sdk.Application, len(verifiedApplications))
	for i := range verifiedApplications {
		apps[i] = *verifiedApplications[i]

		// By default all vcds_strategy password are masked
		apps[i].RepositoryStrategy.Password = sdk.PasswordPlaceholder
	}

	return apps, nil
}

func get(ctx context.Context, db gorp.SqlExecutor, query gorpmapping.Query, opts ...LoadOptionFunc) (*sdk.Application, error) {
	app, err := getWithClearVCSStrategyPassword(ctx, db, query, opts...)
	if err != nil {
		return nil, err
	}
	app.RepositoryStrategy.Password = sdk.PasswordPlaceholder
	app.RepositoryStrategy.SSHKeyContent = ""
	return app, nil
}

func getWithClearVCSStrategyPassword(ctx context.Context, db gorp.SqlExecutor, query gorpmapping.Query, opts ...LoadOptionFunc) (*sdk.Application, error) {
	dbApp := dbApplication{}

	// Allways load with decryption to get all the data for vcs_strategy
	found, err := gorpmapping.Get(ctx, db, query, &dbApp, gorpmapping.GetOptions.WithDecryption)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, sdk.WithStack(sdk.ErrNotFound)
	}

	isValid, err := gorpmapping.CheckSignature(dbApp, dbApp.Signature)
	if err != nil {
		return nil, err
	}
	if !isValid {
		log.Error(context.Background(), "application.get> application %d data corrupted", dbApp.ID)
		return nil, sdk.WithStack(sdk.ErrNotFound)
	}

	app := dbApp.Application

	for i := range opts {
		if err := opts[i](ctx, db, &app); err != nil {
			return nil, err
		}
	}

	return &app, nil
}

// Exists checks if an application given its name exists
func Exists(db gorp.SqlExecutor, projectKey, appName string) (bool, error) {
	count, err := db.SelectInt("SELECT count(1) FROM application join project ON project.id = application.project_id WHERE project.projectkey = $1 AND application.name = $2", projectKey, appName)
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

// LoadAllByProjectIDAndRepository load all application where repository match given one.
func LoadAllByProjectIDAndRepository(ctx context.Context, db gorp.SqlExecutor, projectID int64, repo string, opts ...LoadOptionFunc) ([]sdk.Application, error) {
	query := gorpmapping.NewQuery(`
    SELECT *
    FROM application
    WHERE project_id = $1
    AND from_repository = $2
  `).Args(projectID, repo)
	return getAll(ctx, db, query, opts...)
}

// LoadByProjectIDAndName load an application from DB.
func LoadByProjectIDAndName(ctx context.Context, db gorp.SqlExecutor, projectID int64, name string, opts ...LoadOptionFunc) (*sdk.Application, error) {
	query := gorpmapping.NewQuery(`
		SELECT *
		FROM application
		WHERE project_id = $1
    AND name = $2
  `).Args(projectID, name)
	return get(ctx, db, query, opts...)
}

// LoadByProjectIDAndNameWithClearVCSStrategyPassword load an application from DB.
func LoadByProjectIDAndNameWithClearVCSStrategyPassword(ctx context.Context, db gorp.SqlExecutor, projectID int64, name string, opts ...LoadOptionFunc) (*sdk.Application, error) {
	query := gorpmapping.NewQuery(`
		SELECT *
		FROM application
		WHERE project_id = $1
    AND name = $2
  `).Args(projectID, name)
	return getWithClearVCSStrategyPassword(ctx, db, query, opts...)
}

// LoadByID load an application from DB.
func LoadByID(ctx context.Context, db gorp.SqlExecutor, id int64, opts ...LoadOptionFunc) (*sdk.Application, error) {
	query := gorpmapping.NewQuery(`
    SELECT *
    FROM application
    WHERE id = $1
  `).Args(id)
	return get(ctx, db, query, opts...)
}

// LoadByIDWithClearVCSStrategyPassword .
func LoadByIDWithClearVCSStrategyPassword(ctx context.Context, db gorp.SqlExecutor, id int64, opts ...LoadOptionFunc) (*sdk.Application, error) {
	query := gorpmapping.NewQuery(`
    SELECT *
    FROM application
    WHERE id = $1
  `).Args(id)
	return getWithClearVCSStrategyPassword(ctx, db, query, opts...)
}

// LoadByWorkflowID loads applications from database for a given workflow id
func LoadByWorkflowID(ctx context.Context, db gorp.SqlExecutor, workflowID int64) ([]sdk.Application, error) {
	query := gorpmapping.NewQuery(`
	  SELECT DISTINCT application.*
	  FROM application
	  JOIN w_node_context ON w_node_context.application_id = application.id
	  JOIN w_node ON w_node.id = w_node_context.node_id
	  JOIN workflow ON workflow.id = w_node.workflow_id
    WHERE workflow.id = $1
  `).Args(workflowID)
	return getAll(ctx, db, query)
}

// Insert add an application id database
func Insert(db gorp.SqlExecutor, projectID int64, app *sdk.Application) error {
	if err := app.IsValid(); err != nil {
		return sdk.WrapError(err, "application is not valid")
	}

	app.ProjectID = projectID
	app.LastModified = time.Now()
	copyVCSStrategy := app.RepositoryStrategy

	dbApp := dbApplication{Application: *app}
	if err := gorpmapping.InsertAndSign(context.Background(), db, &dbApp); err != nil {
		return sdk.WrapError(err, "application.Insert %s(%d)", app.Name, app.ID)
	}
	*app = dbApp.Application
	// Reset the vcs_stragegy except the passowrd because it as been erased by the encryption layed
	app.RepositoryStrategy = copyVCSStrategy
	app.RepositoryStrategy.Password = sdk.PasswordPlaceholder
	app.RepositoryStrategy.SSHKeyContent = ""

	return nil
}

// UpdateColumns is only available for migration, it should be removed in a further release
func UpdateColumns(db gorp.SqlExecutor, app *sdk.Application, columnFilter gorp.ColumnFilter) error {
	app.LastModified = time.Now()
	dbApp := dbApplication{Application: *app}
	if err := gorpmapping.UpdateColumnsAndSign(context.Background(), db, &dbApp, columnFilter); err != nil {
		return sdk.WrapError(err, "application.Update %s(%d)", app.Name, app.ID)
	}
	app.RepositoryStrategy.Password = sdk.PasswordPlaceholder
	app.RepositoryStrategy.SSHKeyContent = ""
	return nil
}

// Update updates application id database
func Update(ctx context.Context, db gorp.SqlExecutor, app *sdk.Application) error {
	if app.RepositoryStrategy.Password == sdk.PasswordPlaceholder {
		appTmp, err := LoadByIDWithClearVCSStrategyPassword(ctx, db, app.ID)
		if err != nil {
			return err
		}
		app.RepositoryStrategy.Password = appTmp.RepositoryStrategy.Password
	}
	if app.RepositoryStrategy.ConnectionType == "ssh" {
		app.RepositoryStrategy.Password = ""
	}

	var copyVCSStrategy = app.RepositoryStrategy

	if err := app.IsValid(); err != nil {
		return sdk.WrapError(err, "application is not valid")
	}
	app.LastModified = time.Now()
	dbApp := dbApplication{Application: *app}
	if err := gorpmapping.UpdateAndSign(context.Background(), db, &dbApp); err != nil {
		return sdk.WrapError(err, "application.Update %s(%d)", app.Name, app.ID)
	}
	// Reset the vcs_stragegy except the passowrd because it as been erased by the encryption layed
	app.RepositoryStrategy = copyVCSStrategy
	app.RepositoryStrategy.Password = sdk.PasswordPlaceholder
	app.RepositoryStrategy.SSHKeyContent = ""
	return nil
}

// LoadAll returns all applications.
func LoadAll(ctx context.Context, db gorp.SqlExecutor, projectID int64, opts ...LoadOptionFunc) ([]sdk.Application, error) {
	query := gorpmapping.NewQuery(`
    SELECT *
    FROM application
    WHERE project_id = $1
    ORDER BY name ASC
  `).Args(projectID)
	return getAll(ctx, db, query, opts...)
}

// LoadAllByIDs returns all applications
func LoadAllByIDs(ctx context.Context, db gorp.SqlExecutor, ids []int64, opts ...LoadOptionFunc) ([]sdk.Application, error) {
	query := gorpmapping.NewQuery(`
	SELECT application.*
	FROM application
	WHERE application.id = ANY($1)
	ORDER BY application.name ASC`).Args(pq.Int64Array(ids))
	return getAll(ctx, db, query, opts...)
}

// LoadAllNames returns all application names
func LoadAllNames(db gorp.SqlExecutor, projectID int64) (sdk.IDNames, error) {
	query := `
		SELECT application.id, application.name, application.description, application.icon
		FROM application
		WHERE application.project_id= $1
		ORDER BY application.name ASC`

	var res sdk.IDNames
	if _, err := db.Select(&res, query, projectID); err != nil {
		if err == sql.ErrNoRows {
			return res, nil
		}
		return nil, sdk.WrapError(err, "application.loadapplicationnames")
	}

	return res, nil
}

// LoadIcon return application icon given his application id
func LoadIcon(db gorp.SqlExecutor, appID int64) (string, error) {
	icon, err := db.SelectStr("SELECT icon FROM application WHERE id = $1", appID)
	return icon, sdk.WithStack(err)
}

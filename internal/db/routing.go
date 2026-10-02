package db

import "database/sql"

// ModelRouteRow is one failover route for a model alias.
type ModelRouteRow struct {
	ID            int64
	ModelName     string
	ProviderID    int64
	UpstreamModel string
	Priority      int
	Weight        int
	Enabled       bool
}

const modelRouteColumns = `id, model_name, provider_id, upstream_model, priority, weight, enabled`

func scanModelRoute(row interface{ Scan(...any) error }) (*ModelRouteRow, error) {
	var (
		route   ModelRouteRow
		enabled int
	)
	if err := row.Scan(&route.ID, &route.ModelName, &route.ProviderID, &route.UpstreamModel, &route.Priority, &route.Weight, &enabled); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	route.Enabled = enabled != 0
	return &route, nil
}

// ListModelRoutesFor returns the enabled routes for one alias, best first.
func ListModelRoutesFor(modelName string) ([]ModelRouteRow, error) {
	rows, err := Get().Query(
		`SELECT `+modelRouteColumns+`
		 FROM model_routes
		 WHERE model_name = ? AND enabled = 1
		 ORDER BY priority ASC, weight DESC`,
		modelName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []ModelRouteRow
	for rows.Next() {
		route, err := scanModelRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, *route)
	}
	return routes, rows.Err()
}

// ListModelRoutes returns every enabled route, best first (newest-safe for the
// admin UI, which never edits them in place yet).
func ListModelRoutes() ([]ModelRouteRow, error) {
	rows, err := Get().Query(
		`SELECT ` + modelRouteColumns + ` FROM model_routes
		 WHERE enabled = 1
		 ORDER BY priority ASC, weight DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []ModelRouteRow
	for rows.Next() {
		route, err := scanModelRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, *route)
	}
	return routes, rows.Err()
}

// NewModelRoute is the insert payload for a failover route.
type NewModelRoute struct {
	ModelName     string
	ProviderID    int64
	UpstreamModel string
	Priority      int
	Weight        int
	Enabled       bool
}

// CreateModelRoute inserts a route and returns the stored row.
func CreateModelRoute(input NewModelRoute) (*ModelRouteRow, error) {
	result, err := Get().Exec(
		`INSERT INTO model_routes (model_name, provider_id, upstream_model, priority, weight, enabled)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		input.ModelName, input.ProviderID, input.UpstreamModel, input.Priority, input.Weight, boolToInt(input.Enabled),
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return scanModelRoute(Get().QueryRow(`SELECT `+modelRouteColumns+` FROM model_routes WHERE id = ?`, id))
}

// DeleteModelRoute removes a route. Reports whether a row was removed.
func DeleteModelRoute(id int64) (bool, error) {
	result, err := Get().Exec(`DELETE FROM model_routes WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// GetModelByName loads an enabled alias (nil when missing/disabled).
func GetModelByName(name string) (*ModelRow, error) {
	return scanModel(Get().QueryRow(`SELECT `+modelColumns+` FROM models WHERE name = ? AND enabled = 1`, name))
}

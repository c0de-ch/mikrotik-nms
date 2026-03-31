package queries

import "database/sql"

func GetSetting(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM app_settings WHERE key=?`, key).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

func SetSetting(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO app_settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value,
	)
	return err
}

func GetAllSettings(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT key, value FROM app_settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	settings := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		settings[k] = v
	}
	return settings, rows.Err()
}

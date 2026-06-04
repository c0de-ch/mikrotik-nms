package routeros

import (
	ros "github.com/go-routeros/routeros/v3"
)

// RawCommand runs a command and returns the raw key-value maps for each reply sentence.
func RawCommand(client *ros.Client, command string) ([]map[string]string, error) {
	reply, err := RunCommand(client, command)
	if err != nil {
		return nil, err
	}
	var results []map[string]string
	for _, re := range reply.Re {
		results = append(results, GetSentenceMap(re))
	}
	return results, nil
}

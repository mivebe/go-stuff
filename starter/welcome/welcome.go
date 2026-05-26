package welcome

import "fmt"

// Welcome returns a welcome message for the named person.
func Welcome(name string) string {
	return fmt.Sprintf("Welcome, %v!", name)
}

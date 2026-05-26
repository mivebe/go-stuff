package other

import "fmt"

// Bye returns a farewell for the named person.
func Bye(name string) string {
	return fmt.Sprintf("Bye, %v. bye!", name)
}

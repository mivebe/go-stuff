package other

import "fmt"

// Hello returns a greeting for the named person.
func Bye(name string) string {
	// Return a greeting that embeds the name in a message.
	message := fmt.Sprintf("Bye, %v. bye!", name)
	return message
}

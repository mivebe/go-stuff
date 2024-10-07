package main

import (
	"fmt"
	"other"
	greeting "starter/ggg"
	"starter/greeting2"

	"rsc.io/quote"
)

func main() {
	fmt.Println(quote.Go())

	fmt.Println(greeting.Hello("Gosho"))
	fmt.Println(greeting2.Hello("Pesho"))
	fmt.Println(other.Bye("Ceco"))
}

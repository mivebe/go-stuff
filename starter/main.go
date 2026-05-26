package main

import (
	"fmt"

	"starter/greeting"
	"starter/other"
	"starter/welcome"

	"rsc.io/quote"
)

func main() {
	fmt.Println(quote.Go())

	fmt.Println(greeting.Hello("Gosho"))
	fmt.Println(welcome.Welcome("Pesho"))
	fmt.Println(other.Bye("Ceco"))
}

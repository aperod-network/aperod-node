// Seed/read a real LevelDB independently of the synthetic HTTP process.
package main

import (
	"fmt"
	"os"

	"github.com/syndtr/goleveldb/leveldb"
)

func main() {
	db, err := leveldb.OpenFile(os.Args[1], nil)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	if len(os.Args) == 2 {
		for _, key := range []string{"b/fixture", "u/fixture", "snapshot/fixture"} {
			if err := db.Put([]byte(key), []byte("retained:"+key), nil); err != nil {
				panic(err)
			}
		}
		return
	}
	for _, key := range []string{"b/fixture", "u/fixture", "snapshot/fixture"} {
		value, err := db.Get([]byte(key), nil)
		if err != nil || string(value) != "retained:"+key {
			panic(fmt.Sprintf("fixture LevelDB data missing: %s", key))
		}
	}
	fmt.Println("retained LevelDB records verified")
}

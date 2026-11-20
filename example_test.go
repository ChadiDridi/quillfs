package quillfs_test

import (
	"fmt"
	"log"
	"os"

	"github.com/ChadiDridi/quillfs"
)

func Example() {
	dir, _ := os.MkdirTemp("", "quillfs-example")
	defer os.RemoveAll(dir)

	fsys, err := quillfs.Open(dir, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer fsys.Close()

	fsys.MkdirAll("notes/2026", 0o755)
	fsys.WriteFile("notes/2026/todo.txt", []byte("write the docs\n"), 0o644)

	f, _ := fsys.OpenFile("notes/2026/todo.txt", os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("ship it\n"))
	f.Sync() // on stable storage from here on
	f.Close()

	fsys.Rename("notes/2026", "notes/done")
	data, _ := fsys.ReadFile("notes/done/todo.txt")
	fmt.Print(string(data))

	entries, _ := fsys.ReadDir("notes")
	for _, e := range entries {
		fmt.Println(e.Name(), e.IsDir())
	}
	// Output:
	// write the docs
	// ship it
	// done true
}

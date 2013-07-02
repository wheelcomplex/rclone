// Sync files and directories to and from local and remote object stores
//
// Nick Craig-Wood <nick@craig-wood.com>
package main

import (
	"./fs"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"
	// Active file systems
	_ "./drive"
	_ "./local"
	_ "./s3"
	_ "./swift"
)

// Globals
var (
	// Flags
	cpuprofile    = flag.String("cpuprofile", "", "Write cpu profile to file")
	verbose       = flag.Bool("verbose", false, "Print lots more stuff")
	quiet         = flag.Bool("quiet", false, "Print as little stuff as possible")
	dry_run       = flag.Bool("dry-run", false, "Do a trial run with no permanent changes")
	checkers      = flag.Int("checkers", 8, "Number of checkers to run in parallel.")
	transfers     = flag.Int("transfers", 4, "Number of file transfers to run in parallel.")
	statsInterval = flag.Duration("stats", time.Minute*1, "Interval to print stats")
	modifyWindow  = flag.Duration("modify-window", time.Nanosecond, "Max time diff to be considered the same")
)

// A pair of fs.Objects
type PairFsObjects struct {
	src, dst fs.Object
}

type PairFsObjectsChan chan PairFsObjects

// Check to see if src needs to be copied to dst and if so puts it in out
func checkOne(src, dst fs.Object, out fs.ObjectsChan) {
	if dst == nil {
		fs.Debug(src, "Couldn't find local file - download")
		out <- src
		return
	}
	// Check to see if can store this
	if !src.Storable() {
		return
	}
	// Check to see if changed or not
	if fs.Equal(src, dst) {
		fs.Debug(src, "Unchanged skipping")
		return
	}
	out <- src
}

// Read FsObjects~s on in send to out if they need uploading
//
// FIXME potentially doing lots of MD5SUMS at once
func PairChecker(in PairFsObjectsChan, out fs.ObjectsChan, wg *sync.WaitGroup) {
	defer wg.Done()
	for pair := range in {
		src := pair.src
		fs.Stats.Checking(src)
		checkOne(src, pair.dst, out)
		fs.Stats.DoneChecking(src)
	}
}

// Read FsObjects~s on in send to out if they need uploading
//
// FIXME potentially doing lots of MD5SUMS at once
func Checker(in, out fs.ObjectsChan, fdst fs.Fs, wg *sync.WaitGroup) {
	defer wg.Done()
	for src := range in {
		fs.Stats.Checking(src)
		dst := fdst.NewFsObject(src.Remote())
		checkOne(src, dst, out)
		fs.Stats.DoneChecking(src)
	}
}

// Read FsObjects on in and copy them
func Copier(in fs.ObjectsChan, fdst fs.Fs, wg *sync.WaitGroup) {
	defer wg.Done()
	for src := range in {
		fs.Stats.Transferring(src)
		fs.Copy(fdst, src)
		fs.Stats.DoneTransferring(src)
	}
}

// Copies fsrc into fdst
func CopyFs(fdst, fsrc fs.Fs) {
	err := fdst.Mkdir()
	if err != nil {
		fs.Stats.Error()
		log.Fatal("Failed to make destination")
	}

	to_be_checked := fsrc.List()
	to_be_uploaded := make(fs.ObjectsChan, *transfers)

	var checkerWg sync.WaitGroup
	checkerWg.Add(*checkers)
	for i := 0; i < *checkers; i++ {
		go Checker(to_be_checked, to_be_uploaded, fdst, &checkerWg)
	}

	var copierWg sync.WaitGroup
	copierWg.Add(*transfers)
	for i := 0; i < *transfers; i++ {
		go Copier(to_be_uploaded, fdst, &copierWg)
	}

	log.Printf("Waiting for checks to finish")
	checkerWg.Wait()
	close(to_be_uploaded)
	log.Printf("Waiting for transfers to finish")
	copierWg.Wait()
}

// Delete all the files passed in the channel
func DeleteFiles(to_be_deleted fs.ObjectsChan) {
	var wg sync.WaitGroup
	wg.Add(*transfers)
	for i := 0; i < *transfers; i++ {
		go func() {
			defer wg.Done()
			for dst := range to_be_deleted {
				if *dry_run {
					fs.Debug(dst, "Not deleting as -dry-run")
				} else {
					fs.Stats.Checking(dst)
					err := dst.Remove()
					fs.Stats.DoneChecking(dst)
					if err != nil {
						fs.Stats.Error()
						fs.Log(dst, "Couldn't delete: %s", err)
					} else {
						fs.Debug(dst, "Deleted")
					}
				}
			}
		}()
	}

	log.Printf("Waiting for deletions to finish")
	wg.Wait()
}

// Syncs fsrc into fdst
func Sync(fdst, fsrc fs.Fs) {
	err := fdst.Mkdir()
	if err != nil {
		fs.Stats.Error()
		log.Fatal("Failed to make destination")
	}

	log.Printf("Building file list")

	// Read the destination files first
	// FIXME could do this in parallel and make it use less memory
	delFiles := make(map[string]fs.Object)
	for dst := range fdst.List() {
		delFiles[dst.Remote()] = dst
	}

	// Read source files checking them off against dest files
	to_be_checked := make(PairFsObjectsChan, *transfers)
	to_be_uploaded := make(fs.ObjectsChan, *transfers)

	var checkerWg sync.WaitGroup
	checkerWg.Add(*checkers)
	for i := 0; i < *checkers; i++ {
		go PairChecker(to_be_checked, to_be_uploaded, &checkerWg)
	}

	var copierWg sync.WaitGroup
	copierWg.Add(*transfers)
	for i := 0; i < *transfers; i++ {
		go Copier(to_be_uploaded, fdst, &copierWg)
	}

	go func() {
		for src := range fsrc.List() {
			remote := src.Remote()
			dst, found := delFiles[remote]
			if found {
				delete(delFiles, remote)
				to_be_checked <- PairFsObjects{src, dst}
			} else {
				// No need to check doesn't exist
				to_be_uploaded <- src
			}
		}
		close(to_be_checked)
	}()

	log.Printf("Waiting for checks to finish")
	checkerWg.Wait()
	close(to_be_uploaded)
	log.Printf("Waiting for transfers to finish")
	copierWg.Wait()

	if fs.Stats.Errored() {
		log.Printf("Not deleting files as there were IO errors")
		return
	}

	// Delete the spare files
	toDelete := make(fs.ObjectsChan, *transfers)
	go func() {
		for _, fs := range delFiles {
			toDelete <- fs
		}
		close(toDelete)
	}()
	DeleteFiles(toDelete)
}

// Checks the files in fsrc and fdst according to Size and MD5SUM
func Check(fdst, fsrc fs.Fs) {
	log.Printf("Building file list")

	// Read the destination files first
	// FIXME could do this in parallel and make it use less memory
	dstFiles := make(map[string]fs.Object)
	for dst := range fdst.List() {
		dstFiles[dst.Remote()] = dst
	}

	// Read the source files checking them against dstFiles
	// FIXME could do this in parallel and make it use less memory
	srcFiles := make(map[string]fs.Object)
	commonFiles := make(map[string][]fs.Object)
	for src := range fsrc.List() {
		remote := src.Remote()
		if dst, ok := dstFiles[remote]; ok {
			commonFiles[remote] = []fs.Object{dst, src}
			delete(dstFiles, remote)
		} else {
			srcFiles[remote] = src
		}
	}

	log.Printf("Files in %s but not in %s", fdst, fsrc)
	for remote := range dstFiles {
		fs.Stats.Error()
		log.Printf(remote)
	}

	log.Printf("Files in %s but not in %s", fsrc, fdst)
	for remote := range srcFiles {
		fs.Stats.Error()
		log.Printf(remote)
	}

	checks := make(chan []fs.Object, *transfers)
	go func() {
		for _, check := range commonFiles {
			checks <- check
		}
		close(checks)
	}()

	var checkerWg sync.WaitGroup
	checkerWg.Add(*checkers)
	for i := 0; i < *checkers; i++ {
		go func() {
			defer checkerWg.Done()
			for check := range checks {
				dst, src := check[0], check[1]
				fs.Stats.Checking(src)
				if src.Size() != dst.Size() {
					fs.Stats.DoneChecking(src)
					fs.Stats.Error()
					fs.Log(src, "Sizes differ")
					continue
				}
				same, err := fs.CheckMd5sums(src, dst)
				fs.Stats.DoneChecking(src)
				if err != nil {
					continue
				}
				if !same {
					fs.Stats.Error()
					fs.Log(src, "Md5sums differ")
				}
				fs.Debug(src, "OK")
			}
		}()
	}

	log.Printf("Waiting for checks to finish")
	checkerWg.Wait()
	log.Printf("%d differences found", fs.Stats.GetErrors())
}

// List the Fs to stdout
//
// Lists in parallel which may get them out of order
func List(f, _ fs.Fs) {
	in := f.List()
	var wg sync.WaitGroup
	wg.Add(*checkers)
	for i := 0; i < *checkers; i++ {
		go func() {
			defer wg.Done()
			for o := range in {
				fs.Stats.Checking(o)
				modTime := o.ModTime()
				fs.Stats.DoneChecking(o)
				fmt.Printf("%9d %19s %s\n", o.Size(), modTime.Format("2006-01-02 15:04:05.00000000"), o.Remote())
			}
		}()
	}
	wg.Wait()
}

// List the directories/buckets/containers in the Fs to stdout
func ListDir(f, _ fs.Fs) {
	for dir := range f.ListDir() {
		fmt.Printf("%12d %13s %9d %s\n", dir.Bytes, dir.When.Format("2006-01-02 15:04:05"), dir.Count, dir.Name)
	}
}

// Makes a destination directory or container
func mkdir(fdst, fsrc fs.Fs) {
	err := fdst.Mkdir()
	if err != nil {
		fs.Stats.Error()
		log.Fatalf("Mkdir failed: %s", err)
	}
}

// Removes a container but not if not empty
func rmdir(fdst, fsrc fs.Fs) {
	if *dry_run {
		log.Printf("Not deleting %s as -dry-run", fdst)
	} else {
		err := fdst.Rmdir()
		if err != nil {
			fs.Stats.Error()
			log.Fatalf("Rmdir failed: %s", err)
		}
	}
}

// Removes a container and all of its contents
//
// FIXME doesn't delete local directories
func purge(fdst, fsrc fs.Fs) {
	if f, ok := fdst.(fs.Purger); ok {
		err := f.Purge()
		if err != nil {
			fs.Stats.Error()
			log.Fatalf("Purge failed: %s", err)
		}
	} else {
		DeleteFiles(fdst.List())
		log.Printf("Deleting path")
		rmdir(fdst, fsrc)
	}
}

type Command struct {
	name             string
	help             string
	run              func(fdst, fsrc fs.Fs)
	minArgs, maxArgs int
}

// checkArgs checks there are enough arguments and prints a message if not
func (cmd *Command) checkArgs(args []string) {
	if len(args) < cmd.minArgs {
		syntaxError()
		fmt.Fprintf(os.Stderr, "Command %s needs %d arguments mininum\n", cmd.name, cmd.minArgs)
		os.Exit(1)
	} else if len(args) > cmd.maxArgs {
		syntaxError()
		fmt.Fprintf(os.Stderr, "Command %s needs %d arguments maximum\n", cmd.name, cmd.maxArgs)
		os.Exit(1)
	}
}

var Commands = []Command{
	{
		"copy",
		`<source> <destination>

        Copy the source to the destination.  Doesn't transfer
        unchanged files, testing first by modification time then by
        MD5SUM.  Doesn't delete files from the destination.

`,
		CopyFs,
		2, 2,
	},
	{
		"sync",
		`<source> <destination>

        Sync the source to the destination.  Doesn't transfer
        unchanged files, testing first by modification time then by
        MD5SUM.  Deletes any files that exist in source that don't
        exist in destination. Since this can cause data loss, test
        first with the -dry-run flag.`,

		Sync,
		2, 2,
	},
	{
		"ls",
		`[<path>]

        List all the objects in the the path.`,

		List,
		1, 1,
	},
	{
		"lsd",
		`[<path>]

        List all directoryes/objects/buckets in the the path.`,

		ListDir,
		1, 1,
	},
	{
		"mkdir",
		`<path>

        Make the path if it doesn't already exist`,

		mkdir,
		1, 1,
	},
	{
		"rmdir",
		`<path>

        Remove the path.  Note that you can't remove a path with
        objects in it, use purge for that.`,

		rmdir,
		1, 1,
	},
	{
		"purge",
		`<path>

        Remove the path and all of its contents.`,

		purge,
		1, 1,
	},
	{
		"check",
		`<source> <destination>

        Checks the files in the source and destination match.  It
        compares sizes and MD5SUMs and prints a report of files which
        don't match.  It doesn't alter the source or destination.`,

		Check,
		2, 2,
	},
	{
		"help",
		`

        This help.`,

		nil,
		0, 0,
	},
}

// syntaxError prints the syntax
func syntaxError() {
	fmt.Fprintf(os.Stderr, `Sync files and directories to and from local and remote object stores

Syntax: [options] subcommand <parameters> <parameters...>

Subcommands:

`)
	for i := range Commands {
		cmd := &Commands[i]
		fmt.Fprintf(os.Stderr, "    %s: %s\n\n", cmd.name, cmd.help)
	}

	fmt.Fprintf(os.Stderr, "Options:\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, `
It is only necessary to use a unique prefix of the subcommand, eg 'up' for 'upload'.
`)
}

// Exit with the message
func fatal(message string, args ...interface{}) {
	syntaxError()
	fmt.Fprintf(os.Stderr, message, args...)
	os.Exit(1)
}

func main() {
	flag.Usage = syntaxError
	flag.Parse()
	args := flag.Args()
	runtime.GOMAXPROCS(runtime.NumCPU())

	// Pass on some flags to fs.Config
	fs.Config.Verbose = *verbose
	fs.Config.Quiet = *quiet
	fs.Config.ModifyWindow = *modifyWindow
	fs.Config.Checkers = *checkers
	fs.Config.Transfers = *transfers

	// Setup profiling if desired
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			fs.Stats.Error()
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if len(args) < 1 {
		fatal("No command supplied\n")
	}

	cmd := strings.ToLower(args[0])
	args = args[1:]

	// Find the command doing a prefix match
	var found *Command
	for i := range Commands {
		command := &Commands[i]
		// exact command name found - use that
		if command.name == cmd {
			found = command
			break
		} else if strings.HasPrefix(command.name, cmd) {
			if found != nil {
				fs.Stats.Error()
				log.Fatalf("Not unique - matches multiple commands %q", cmd)
			}
			found = command
		}
	}
	if found == nil {
		fs.Stats.Error()
		log.Fatalf("Unknown command %q", cmd)
	}
	found.checkArgs(args)

	// Make source and destination fs
	var fdst, fsrc fs.Fs
	var err error
	if len(args) >= 1 {
		fdst, err = fs.NewFs(args[0])
		if err != nil {
			fs.Stats.Error()
			log.Fatal("Failed to create file system: ", err)
		}
	}
	if len(args) >= 2 {
		fsrc, err = fs.NewFs(args[1])
		if err != nil {
			fs.Stats.Error()
			log.Fatal("Failed to create destination file system: ", err)
		}
		fsrc, fdst = fdst, fsrc
	}

	// Work out modify window
	if fsrc != nil {
		precision := fsrc.Precision()
		log.Printf("Source precision %s\n", precision)
		if precision > fs.Config.ModifyWindow {
			fs.Config.ModifyWindow = precision
		}
	}
	if fdst != nil {
		precision := fdst.Precision()
		log.Printf("Destination precision %s\n", precision)
		if precision > fs.Config.ModifyWindow {
			fs.Config.ModifyWindow = precision
		}
	}
	log.Printf("Modify window is %s\n", fs.Config.ModifyWindow)

	// Print the stats every statsInterval
	go func() {
		ch := time.Tick(*statsInterval)
		for {
			<-ch
			fs.Stats.Log()
		}
	}()

	// Run the actual command
	if found.run != nil {
		found.run(fdst, fsrc)
		fmt.Println(fs.Stats)
		log.Printf("*** Go routines at exit %d\n", runtime.NumGoroutine())
		if fs.Stats.Errored() {
			os.Exit(1)
		}
		os.Exit(0)
	} else {
		syntaxError()
	}

}
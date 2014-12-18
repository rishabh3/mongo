package mongofiles

import (
	"fmt"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/mongodb/mongo-tools/common/util"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"io"
	"os"
	"regexp"
	"time"
)

const (
	// list of possible commands for mongofiles
	List   = "list"
	Search = "search"
	Put    = "put"
	Get    = "get"
	Delete = "delete"
)

type MongoFiles struct {
	// generic mongo tool options
	ToolOptions *options.ToolOptions

	// mongofiles-specific storage options
	StorageOptions *StorageOptions

	// for connecting to the db
	SessionProvider *db.SessionProvider

	// command to run
	Command string
	// filename in GridFS
	FileName string
}

// represents a GridFS file
type GFSFile struct {
	Id          bson.ObjectId `bson:"_id"`
	ChunkSize   int           `bson:"chunkSize"`
	Name        string        `bson:"filename"`
	Length      int64         `bson:"length"`
	Md5         string        `bson:"md5"`
	UploadDate  time.Time     `bson:"uploadDate"`
	ContentType string        `bson:"contentType,omitempty"`
}

func (mf *MongoFiles) ValidateCommand(args []string) error {
	// make sure a command is specified and that we don't have
	// too many arguments
	if len(args) == 0 {
		return fmt.Errorf("no command specified")
	} else if len(args) > 2 {
		return fmt.Errorf("too many positional arguments")
	}

	var fileName string
	switch args[0] {
	case List:
		if len(args) == 1 {
			fileName = ""
		} else {
			fileName = args[1]
		}
	case Search, Put, Get, Delete:
		// also make sure the supporting argument isn't literally an empty string
		// for example, mongofiles get ""
		if len(args) == 1 || args[1] == "" {
			return fmt.Errorf("'%v' argument missing", args[0])
		}
		fileName = args[1]
	default:
		return fmt.Errorf("'%v' is not a valid command", args[0])
	}

	if mf.StorageOptions.GridFSPrefix == "" {
		return fmt.Errorf("--prefix can not be blank")
	}

	// set the mongofiles command and file name
	mf.Command = args[0]
	mf.FileName = fileName
	return nil
}

// query GridFS for files and display the results
func (self *MongoFiles) findAndDisplay(gfs *mgo.GridFS, query bson.M) (string, error) {
	display := ""

	cursor := gfs.Find(query).Iter()
	defer cursor.Close()

	var file GFSFile
	for cursor.Next(&file) {
		display += fmt.Sprintf("%s\t%d\n", file.Name, file.Length)
	}
	if err := cursor.Err(); err != nil {
		return "", fmt.Errorf("error retrieving list of GridFS files: %v", err)
	}

	return display, nil
}

// Return local file (set by --local optional flag) name (or default to self.FileName)
func (self *MongoFiles) getLocalFileName() string {
	localFileName := self.StorageOptions.LocalFileName
	if localFileName == "" {
		localFileName = self.FileName
	}
	return localFileName
}

// handle logic for 'get' command
func (self *MongoFiles) handleGet(gfs *mgo.GridFS) (string, error) {
	gFile, err := gfs.Open(self.FileName)
	if err != nil {
		return "", fmt.Errorf("error opening GridFS file '%s': %v", self.FileName, err)
	}
	defer gFile.Close()

	localFileName := self.getLocalFileName()
	localFile, err := os.Create(localFileName)
	if err != nil {
		return "", fmt.Errorf("error while opening local file '%v': %v\n", localFileName, err)
	}
	defer localFile.Close()
	log.Logf(log.DebugLow, "created local file '%v'", localFileName)

	_, err = io.Copy(localFile, gFile)
	if err != nil {
		return "", fmt.Errorf("error while writing data into local file '%v': %v\n", localFileName, err)
	}

	return fmt.Sprintf("Finished writing to: %s\n", localFileName), nil
}

// handle logic for 'put' command
func (self *MongoFiles) handlePut(gfs *mgo.GridFS) (string, error) {
	localFileName := self.getLocalFileName()

	var output string

	// check if --replace flag turned on
	if self.StorageOptions.Replace {
		err := gfs.Remove(self.FileName)
		if err != nil {
			return "", err
		}
		output = fmt.Sprintf("removed all instances of '%v' from GridFS\n", self.FileName)
	}

	localFile, err := os.Open(localFileName)
	if err != nil {
		return "", fmt.Errorf("error while opening local file '%v' : %v\n", localFileName, err)
	}
	defer localFile.Close()
	log.Logf(log.DebugLow, "creating GridFS file '%v' from local file '%v'", self.FileName, localFileName)

	gFile, err := gfs.Create(self.FileName)
	if err != nil {
		return "", fmt.Errorf("error while creating '%v' in GridFS: %v\n", self.FileName, err)
	}
	defer gFile.Close()

	// set optional mime type
	if self.StorageOptions.ContentType != "" {
		gFile.SetContentType(self.StorageOptions.ContentType)
	}

	_, err = io.Copy(gFile, localFile)
	if err != nil {
		return "", fmt.Errorf("error while storing '%v' into GridFS: %v\n", localFileName, err)
	}

	output += fmt.Sprintf("added file: %v\n", gFile.Name())
	return output, nil
}

// Run the mongofiles utility
func (self *MongoFiles) Run(displayConnUrl bool) (string, error) {
	connUrl := self.ToolOptions.Host
	if connUrl == "" {
		connUrl = util.DefaultHost
	}
	if self.ToolOptions.Port != "" {
		connUrl = fmt.Sprintf("%s:%s", connUrl, self.ToolOptions.Port)
	}

	// get session
	session, err := self.SessionProvider.GetSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	// check if we are using a replica set and fall back to w=1 if we aren't (for <= 2.4)
	isRepl, err := self.SessionProvider.IsReplicaSet()
	if err != nil {
		return "", fmt.Errorf("error determining if connected to replica set: %v", err)
	}

	safety, err := db.BuildWriteConcern(self.StorageOptions.WriteConcern, isRepl)
	if err != nil {
		return "", fmt.Errorf("error parsing write concern: %v", err)
	}

	// configure the session with the appropriate write concern and ensure the
	// socket does not timeout
	session.SetSafe(safety)
	session.SetSocketTimeout(0)

	if displayConnUrl {
		log.Logf(log.Always, "connected to: %v\n", connUrl)
	}

	// first validate the namespaces we'll be using: <db>.<prefix>.files and <db>.<prefix>.chunks
	// it's ok to validate only <db>.<prefix>.chunks (the longer one)
	err = util.ValidateFullNamespace(fmt.Sprintf("%s.%s.chunks", self.StorageOptions.DB,
		self.StorageOptions.GridFSPrefix))

	if err != nil {
		return "", err
	}
	// get GridFS handle
	gfs := session.DB(self.StorageOptions.DB).GridFS(self.StorageOptions.GridFSPrefix)

	var output string

	log.Logf(log.Info, "handling mongofiles '%v' command...", self.Command)

	switch self.Command {

	case List:

		query := bson.M{}
		if self.FileName != "" {
			regex := bson.M{"$regex": "^" + regexp.QuoteMeta(self.FileName)}
			query = bson.M{"filename": regex}
		}

		output, err = self.findAndDisplay(gfs, query)
		if err != nil {
			return "", err
		}

	case Search:

		regex := bson.M{"$regex": self.FileName}
		query := bson.M{"filename": regex}

		output, err = self.findAndDisplay(gfs, query)
		if err != nil {
			return "", err
		}

	case Get:

		output, err = self.handleGet(gfs)
		if err != nil {
			return "", err
		}

	case Put:

		output, err = self.handlePut(gfs)
		if err != nil {
			return "", err
		}

	case Delete:

		err = gfs.Remove(self.FileName)
		if err != nil {
			return "", fmt.Errorf("error while removing '%v' from GridFS: %v\n", self.FileName, err)
		}
		output = fmt.Sprintf("successfully deleted all instances of '%v' from GridFS\n", self.FileName)

	}

	return output, nil
}

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/hashicorp/golang-lru/v2"
	"golang.org/x/exp/slices"
)

func main() {
	log.SetFlags(0)

	addr := flag.String("addr", "127.0.0.1:8000", "Address to listen on")
	wordsPath := flag.String(
		"words", "",
		"Path to file of sorted, valid words (valid meaning only contains ASCII letters)",
	)
	logPath := flag.String("log", "", "Path to log file (empty is stderr)")
	indexPath := flag.String(
		"index", "index.html",
		"Path to index.html (or similar) file",
	)
	flag.Parse()

	srvr, err := newServer(*addr, *wordsPath, *indexPath)
	if err != nil {
		log.Fatal("error creating server: ", err)
	}
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatal("error opening log file: ", err)
		}
		log.SetOutput(f)
	}
	log.Fatal(srvr.Run())
}

type server struct {
	words     *Words
	srvr      *http.Server
	indexPath string
	// Caches the sorted letters and JSON encoded sorted array of words combos.
	cache *lru.TwoQueueCache[string, []byte]
}

func newServer(addr, wordsPath, indexPath string) (*server, error) {
	words, err := loadWords(wordsPath)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(indexPath); err != nil {
		return nil, err
	}
	cache, err := lru.New2Q[string, []byte](100)
	return &server{
		words: words,
		srvr: &http.Server{
			Addr: addr,
		},
		indexPath: indexPath,
		cache:     cache,
	}, err
}

func (s *server) Run() error {
	r := http.NewServeMux()
	r.HandleFunc("/", s.homeHandler)
	r.HandleFunc("/words", s.getWordsHandler)
	s.srvr.Handler = r
	log.Print("Running server on ", s.srvr.Addr)
	return s.srvr.ListenAndServe()
}

func (s *server) homeHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, s.indexPath)
}

func (s *server) getWordsHandler(w http.ResponseWriter, r *http.Request) {
	letters := sortLetters(r.URL.Query().Get("letters"))
	if letters == "" {
		http.Error(w, "invalid letters", http.StatusBadRequest)
		return
	}
	wordsJSON, ok := s.cache.Get(letters)
	if ok {
		w.Write(wordsJSON)
		return
	}
	words := s.getWords(letters)
	wordsJSON, err := json.Marshal(words)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Write(wordsJSON)
	s.cache.Add(letters, wordsJSON)
}

// Expects the letters to be valid (see sortLetters).
func (s *server) getWords(letters string) []string {
	l, found, prev := len(letters), []string{}, byte(0)
	wordsLen := len(s.words.words)
	for i := 0; i < l; i++ {
		b := letters[i]
		// Skip letters that have already been done.
		if prev == b {
			continue
		}
		for index := s.words.letterIndexes[b-'a']; index < wordsLen; index++ {
			word := s.words.words[index]
			// If we have moved on to the next letter in the alphabet in the words
			// list, go to the next letter in letters.
			if word.word[0] != b {
				break
			}
			if word.canMakeFrom(letters) {
				found = append(found, word.word)
			}
		}
		prev = b
	}
	return found
}

type Word struct {
	// letters is the letters of the word but sorted. This would be used to check
	// against an input of sorted letters. Ex) If the word is "cab" and the input
	// is "aabbcc", cab would be matched as follows:
	// "cab" letters are "abc". Then, check these letters against the input
	// letters, 'a' matches with 'a', 'b' doesn't match with the next 'a' but
	// matches with 'b' (next letter of input), 'c' also doesn't match with 'b'
	// (next letter of input) but matches with 'c'.
	word, letters string
}

// Expects valid input (see sortLetters).
func (w Word) canMakeFrom(input string) bool {
	ll, il := len(w.letters), len(input)
	if ll > il {
		return false
	}
	for li, ii := 0, 0; ii < il; ii++ {
		lb, ib := w.letters[li], input[ii]
		if lb == ib {
			li++
		} else if lb < ib {
			// Return since the only case where the input letter is greater than the
			// letters letter is when the latter isn't present in the input.
			return false
		}
		// End of the letters reached.
		if li == ll {
			return true
		}
	}
	return false
}

type Words struct {
	// Keep it sorted by Word.word rather than Word.letters so that way there
	// isn't a more disproportionate amount of words (groups of letters)
	// starting with 'a'.
	words []Word
	// Indexes of the the first words that start with a given letter.
	// 'a' corresponds to 0 (in this array), 'b' to 1, etc.
	letterIndexes [26]int
}

// Expects the file to contain valid, sorted words
func loadWords(fpath string) (*Words, error) {
	f, err := os.Open(fpath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	words, r, prev := &Words{}, bufio.NewReader(f), byte(0)
	for index := 0; true; index++ {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return words, nil
			}
			return words, err
		}
		wordStr := strings.TrimSpace(line)
		letters := sortLetters(wordStr)
		if letters == "" {
			return words, fmt.Errorf("invalid word at lineno %d", index+1)
		}
		word := Word{word: wordStr, letters: letters}
		words.words = append(words.words, word)
		// Add the index when new first letter is encountered.
		if b := line[0]; b != prev {
			words.letterIndexes[b-'a'] = index
			prev = b
		}
	}
	// Unreachable.
	return words, nil
}

// Returns the sorted letters in lowercase. If there is a non-letter, an empty
// string is returned. If the string is sorted, contains only letters, and is
// all lowercase, the same string is returned without making any extra
// allocations. Allocations will be made otherwise since the string needs to be
// converted to and from a byte slice to make changes, whihc will make
// allocations.
func sortLetters(letters string) string {
	l := len(letters)
	shouldSort, shouldLower := false, false
	for i := 1; i < l; i++ {
		b := letters[i]
		/*
		   if b < 'a' {
		     if b >= 'A' && b <= 'Z' {
		       // Convert the variable to uppercase.
		       b += 'a' - 'A'
		       shouldLower = true
		     } else {
		       // Not a letter.
		       return ""
		     }
		   } else if b > 'z' {
		     // Not a letter.
		     return ""
		   }
		*/
		if !(b >= 'a' && b <= 'z') {
			if b >= 'A' && b <= 'Z' {
				// Convert the variable to lowercase.
				b += 'a' - 'A'
				shouldLower = true
			} else {
				// Not a letter.
				return ""
			}
		}
		if b < letters[i-1] {
			shouldSort = true
		}
	}
	if shouldLower {
		letters = strings.ToLower(letters)
	}
	if shouldSort {
		// Only sort if necessary since the string needs to be converted to bytes
		// and back, and Go allocates on the conversions.
		bytes := []byte(letters)
		slices.Sort(bytes)
		letters = string(bytes)
	}
	return letters
}

/*

type Words struct {
  words []string
  letterIndexes [26]int
}

func loadWords(fpath string) (Words, error) {
  words := Words{}
  f, err := os.Open(fpath)
  if err != nil {
    return words, err
  }
  defer f.Close()
  r := bufio.NewReader(f)
  for index := 0; true; index++ {
    line, err := r.ReadString('\n')
    if err != nil {
      if err == io.EOF {
        return words, nil
      }
      return words, err
    }
    words.words = append(words.words, strings.TrimSpace(line))
    // Add the index when new first letter is encountered.
    i := int(line[0] - 'a')
    if i == len(words.letterIndexes) {
      words.letterIndexes[i] = index
    }
  }
  // Unreachable
  return words, nil
}

*/

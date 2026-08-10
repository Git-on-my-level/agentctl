package ids

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Version 1 vendors the official BIP-39 English list, in its published order:
// https://github.com/bitcoin/bips/blob/master/bip-0039/english.txt
// The digest covers the exact embedded bytes, including its trailing newline.
//
//go:embed wordlist.txt
var wordListText string

const WordListSHA256 = "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda"

var (
	wordList    []string
	wordIndex   map[string]uint16
	wordPattern = regexp.MustCompile(`^[a-z]{3,10}$`)
)

func init() {
	sum := sha256.Sum256([]byte(wordListText))
	if hex.EncodeToString(sum[:]) != WordListSHA256 {
		panic("agentctl: word list v1 digest mismatch")
	}
	wordList = strings.Split(strings.TrimSuffix(wordListText, "\n"), "\n")
	if len(wordList) != 2048 {
		panic(fmt.Sprintf("agentctl: word list v1 has %d words", len(wordList)))
	}
	wordIndex = make(map[string]uint16, len(wordList))
	for i, word := range wordList {
		if !wordPattern.MatchString(word) {
			panic(fmt.Sprintf("agentctl: invalid word %q at index %d", word, i))
		}
		if _, exists := wordIndex[word]; exists {
			panic(fmt.Sprintf("agentctl: duplicate word %q", word))
		}
		wordIndex[word] = uint16(i)
	}
}

func WordListDigest() string { return "sha256:" + WordListSHA256 }

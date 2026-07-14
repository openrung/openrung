package relayruntime

import (
	"crypto/rand"
	"math/big"
)

// labelAdjectives and labelNouns are combined into friendly relay labels like
// "happy-hippo". Keep both lists lowercase and within the safe label charset
// (see relay.NormalizeLabel). The two lists are intentionally large so the
// adjective-noun namespace is wide and accidental collisions between
// independently named relays stay unlikely.
var labelAdjectives = []string{
	"amber", "ample", "arctic", "autumn", "balmy", "bold", "bouncy", "brave",
	"breezy", "bright", "bubbly", "calm", "candid", "cheery", "chipper", "clever",
	"cosmic", "crisp", "crystal", "cuddly", "dapper", "daring", "dashing", "dewy",
	"dreamy", "dusky", "eager", "earnest", "electric", "fancy", "fearless", "feisty",
	"fierce", "fleet", "fond", "frosty", "fuzzy", "gallant", "gentle", "giddy",
	"gleeful", "glorious", "golden", "graceful", "grand", "grumpy", "happy", "hardy",
	"hearty", "humble", "icy", "jaunty", "jolly", "keen", "kindly", "lively",
	"lofty", "loyal", "lucky", "lunar", "lush", "mellow", "merry", "mighty",
	"mild", "minty", "misty", "modest", "mossy", "mystic", "nimble", "noble",
	"peppy", "perky", "placid", "plucky", "polished", "prime", "prismatic", "proud",
	"quaint", "quiet", "quirky", "radiant", "rapid", "regal", "robust", "rosy",
	"royal", "rustic", "sandy", "savvy", "serene", "shiny", "silly", "sleek",
	"sleepy", "smooth", "snappy", "snug", "solar", "sparkly", "spirited", "splendid",
	"spotless", "spry", "stellar", "stormy", "sturdy", "suave", "sublime", "sunny",
	"supple", "swift", "tender", "tidy", "tranquil", "trusty", "twinkly", "upbeat",
	"valiant", "velvet", "verdant", "vibrant", "vivid", "warm", "whimsical", "wild",
	"wise", "witty", "zealous", "zesty", "zippy",
}

var labelNouns = []string{
	"alpaca", "anchor", "antelope", "aspen", "badger", "beacon", "birch", "bison",
	"bobcat", "boulder", "brook", "buffalo", "cactus", "camel", "canyon", "caribou",
	"castle", "cedar", "cheetah", "chipmunk", "cliff", "clover", "cobra", "comet",
	"cougar", "cove", "coyote", "crane", "cricket", "dingo", "dolphin", "donkey",
	"dune", "eagle", "egret", "elk", "ember", "falcon", "fern", "ferret",
	"finch", "fjord", "flamingo", "forest", "gazelle", "gecko", "geyser", "glacier",
	"gopher", "grove", "hamster", "harbor", "hawk", "hedgehog", "heron", "hippo",
	"hollow", "ibex", "iguana", "impala", "island", "jackal", "jaguar", "juniper",
	"kestrel", "kingfisher", "kiwi", "koala", "lagoon", "lantern", "lark", "ledge",
	"lemur", "leopard", "lizard", "llama", "lobster", "lynx", "macaw", "magpie",
	"mallard", "manatee", "mantis", "maple", "marmot", "meadow", "meerkat", "mesa",
	"mongoose", "moose", "narwhal", "nebula", "newt", "oasis", "ocelot", "orchard",
	"oriole", "osprey", "otter", "owl", "oyster", "panther", "parrot", "pebble",
	"pelican", "penguin", "pheasant", "pigeon", "platypus", "prairie", "puffin", "python",
	"quartz", "quokka", "rabbit", "raccoon", "raven", "reef", "ridge", "river",
	"salmon", "seal", "sloth", "sparrow", "squirrel", "starling", "stingray", "stoat",
	"stork", "summit", "swan", "tapir", "thistle", "toucan", "tundra", "turtle",
	"urchin", "valley", "viper", "vole", "vulture", "wallaby", "walrus", "warbler",
	"weasel", "whale", "willow", "wombat", "woodpecker", "wren", "yak", "zebra",
}

// GenerateLabel returns a random "adjective-noun" relay label (e.g.
// "happy-hippo"). Labels are cosmetic and not guaranteed unique.
func GenerateLabel() string {
	return labelPick(labelAdjectives) + "-" + labelPick(labelNouns)
}

func labelPick(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return words[0]
	}
	return words[n.Int64()]
}

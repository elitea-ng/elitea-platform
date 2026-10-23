package migrate

import (
	"crypto/subtle"
	"encoding/hex"
)

// released is the text a rewritten migration was released with.
type released struct {
	// checksum is the sha256 of the released file, as deployed ledgers record it.
	checksum string
	// executable is the sha256 of its executable text (executableSQL in the
	// tests), which the current file must still have.
	executable string
}

// releasedTexts lists the applied migrations whose files were rewritten after
// release, by scope and version. A ledger row is immutable, so a database that
// applied the released text keeps its checksum, and its name for the two that
// were renamed; the runner accepts that row as applied and does not re-run it.
// Any other checksum is still refused, as for every migration not listed here.
//
// The rewrite (#1405) removed the legacy runtime's name from comments: the
// executable text of every file below is the released one, which
// TestReleasedTextsDifferOnlyInComments proves. A rewrite that changes what a
// migration does is never listed here; it is a new migration.
var releasedTexts = map[Scope]map[int64]released{
	ScopeShared: {
		49: {
			checksum:   "7bcdcccd01841fd8a830377529668d3a513db31a94755ae97590aafbdf3c3aa6",
			executable: "c0bc548d7d23899e732271c8b50a59cdec22ca51ca9fe9e9070b8f567315a249",
		},
		60: {
			checksum:   "e968dfc42ace1969e214ded0cfc5d837e4451338fd997301993622dde1008b6d",
			executable: "d22f8325c9ce5e0a62bba94e57fe472a5a93846ba315ad7fc74bf96a932bdb5f",
		},
		61: {
			checksum:   "32b016c93b471bdb71f7a4d7448a7f359a82c0e0aae4fa48754eaee68b5afbd9",
			executable: "36dd2dab881e6a7ad3c0d74ef0e1e7d402c232879dc61ec43ebae90a926bab40",
		},
		62: {
			checksum:   "0226677da0a9d612f061bdf221da92566ac0aec5ad7152502a9e5009b6f71903",
			executable: "51e0a046c4e6e189e308c76af82d86c1e8904d83000e9aefff612e3284169b4e",
		},
		64: {
			checksum:   "d725c62d1ed74b8f140bf35124f2be723da0c356d401c3433e586ee0daaa5971",
			executable: "b8df63330f89f40117576cdaff8a7917be5f5a90acaf74c9163e3218156079bd",
		},
		65: {
			checksum:   "72f4c30c79d4d8ef5fa99e88f4e11652b71fb40f2bede792fd026fe45264c764",
			executable: "9fe017d51f121000e49baeecda13dcc99d57f98308e3336aa2157db1abed14b1",
		},
		66: {
			checksum:   "aba2ed0e25fc55536613e891345247e954d4da8ec9ce5bd2d58444dd14342aaa",
			executable: "9317247bfc51800fd08650251faadaa3c064bd5253183f6306119b19ac274936",
		},
		68: {
			checksum:   "43dec9d1cffb835106ae3b5e8a12ffc1eedb5faa01c5ffb6f6e97503b6ae2021",
			executable: "c28d4b45cbf0606924c4e865816165383bf24266a0e2f4dc07321e743802659f",
		},
		69: {
			checksum:   "a9abe8f506f419cfc00108b1327fb20a954b3a45e663f3907da56eb9c318cfec",
			executable: "d0821b6c81576f2ea2aeb9b87c206e1cd316b65be6007a94a5cb58761f841702",
		},
		70: {
			checksum:   "da35ff1aa65422cc9e9438fa7b44ffb4b2399a5db7b6219a960ebfe64a49425b",
			executable: "29afe4d77c8ba26817f49a4a1b701b37391a6a789ff6ae1febaa8eb6b6ce1ddd",
		},
		71: {
			checksum:   "8ccbfbcf2029434656d726e029a43730dbf77b86c073fcee530edef8ee862c63",
			executable: "cb32c2b4e6945402d6876781c1f39d21205c27df7a6687911d0c518eb465cb36",
		},
		72: {
			checksum:   "ba1820c3f0f0edfa0adb61c5310dd653f4a0e968ed5c2be446ef3da8b24ac407",
			executable: "33adf8ac72291cadbd594b081c74df8cbb760b8bcfd82afe3bb9b49cd0df037c",
		},
		73: {
			checksum:   "0c39f60c3177e3a4aba64497df28d0f85e7c50fbe46819e696e16856881ca691",
			executable: "a8a7527a79a07f59187ba181b3306503c116fb2d8e0346f530ccd06a955b1272",
		},
		74: {
			checksum:   "8867b30bbab6b2965fd5820163785562b24618f81c747fc68595210a7066ad11",
			executable: "ff46d0f7e4024cfee461a236cd97d029ef88ec9d36b494412287e02661db3452",
		},
		75: {
			checksum:   "0d3370fa972826a8aeb2eea6f70b642f9ed0dac5aa2200e4b8eb346229eee0c8",
			executable: "8cf8bdeceb26e10538d3313c22ef47fb25dda5f6f708365f3b9294d02f71efd4",
		},
		76: {
			checksum:   "bd8e9907a479d9daf8e5d988b22cd94457566bff3ceaef2615cbe3da42b1bdc1",
			executable: "5e3fcc57b1a0695ade6476f21bb7defd1136259165e24cc784c028f07bcd5e93",
		},
		77: {
			checksum:   "d5e5e9877f0d7ce5fa33f5d4e66f42142e8fb3a0ed2aadfad4ffc053a242512f",
			executable: "b0d7660e481c76526595c0835ce8c862e19f01a62d07d50e0618b1f4cb8a684f",
		},
		78: {
			checksum:   "d12bfb63fe427b4f1427ce11f4c51727f4abdee32b84e6a3b80afae27eb1d13c",
			executable: "2673a2513324c65dee96b8a366858a1a2b38902575650de09ea49d09b00bf5d8",
		},
		79: {
			checksum:   "2b850df8eb5b94fc12cff48ee48efde23ff9d3fc982b100b0a6f6446ebcb573c",
			executable: "6dc76c77aafa6925e9b5c3ada2a91294120886f03d6f7a4e20fede89169cc73f",
		},
		80: {
			checksum:   "53c41cb1e241464cd0b19356546474623c485a47e391e940b42ddec07d1ff3ed",
			executable: "d36ae509b38fbbf242b0f452981b2e86cbd656a61e48b815a5b453a7c67dabf5",
		},
		81: {
			checksum:   "0e670c54930f6bbc6b8ebcaf6fdbd9068e88940755327d7994c9513a6eae9e7b",
			executable: "2b20e6602b3dbe3010c3d0d2f13acfa4c3bc209bf543083297ab56a760c196ab",
		},
		83: {
			checksum:   "ccf5b46770a978772b78927d50efd8cf8dcb999a0f847c319c3ab4ea0d8c26a6",
			executable: "467df202cb75598ec3f380c64f36bc7a6d16a4979916b86198a6d833ac93f920",
		},
		88: {
			checksum:   "5ef26ac187d2b6d7960ba53913999aa3a07a74e0a81dafd04f390c7e30da7983",
			executable: "96e3fe8de2ae2f551dab5153a5de413d21e73be1e05cd4cace7c65eadb7404b2",
		},
		89: {
			checksum:   "d34c039ca184cc98c3f64cba52a86449df97aedf758967798d8d5991e5369f7c",
			executable: "64208c5c83e02433390d426faf41bd7da67d1443ead6d8e4a7a50c78fc07bf07",
		},
		90: {
			checksum:   "7d68618f94ea100686f398169d00cd20dcb876df0821f1510d1fac81685acdda",
			executable: "68c3c8cb8dd6e17ba18469b209d8a85281267683e1ab65285663f5c67f01f207",
		},
		91: {
			checksum:   "fdae5c672b4566818f064ef366b089dff9ee0bcd3eed956d04b44d477a03c6f3",
			executable: "1ed9ed65ba698a8b5734e89d970d07358331f81fe4d148316f19565a1a77a361",
		},
		94: {
			checksum:   "278f1a31c2c72ad4db72eb93ee29359cb4d7e21da719c58f2ff6d3597c07a0ea",
			executable: "0169bae6e12908508e99122accbbbeed47c236995a444618bafffd43c856a9af",
		},
		95: {
			checksum:   "dd0ac6ec96c2642846cd27cf88ea04240d96fab757e5d80b6d7b667cd58fac0a",
			executable: "91d05493852cf6e5e0ec7673302954c47e3b78a1ede87ca6edf6e0f75c4389b1",
		},
		96: {
			checksum:   "b43acd711eca41e9ffa91053161f744602315e564c30a2076016ebe5671d7b83",
			executable: "99c323d59a50096d66357b3ba0c36e3ed908d405407dd2a06c1e31b8b1ff81e9",
		},
		98: {
			checksum:   "c2fad58dfb32532e808daf8a07a26c4641ab85238d8510000fc76160915ffff5",
			executable: "d7f2bf172ca33e5047bfd45f033699e186e8a3d60f186a58c276a02169579bbc",
		},
		100: {
			checksum:   "27bb3ed079534da29d68025799b48a862088b92c394b5d9d828adb6733de2498",
			executable: "0e1f7574a9f17326bb4582501486f95cd9d38a9a352208b919583962f4710d04",
		},
		102: {
			checksum:   "5f20aea35961920b4863ab67e719d17af872265724722915cfce24e2b0c17ab6",
			executable: "a3b1a90ca808343c9fa19ad0b42566cb685822d653950f735f16f77bf8f046c2",
		},
		103: {
			checksum:   "613e7eec3f090669ae9789ff9c62a9be55883b91ef5638c38bb3c2394bc1a9c3",
			executable: "c9f867b44ac2d440e85d84d86e0136012f4862d79c6d576b37f421cc46e65c6c",
		},
		104: {
			checksum:   "effed2571d3d8e8ede30e7a2e986bf07f9cb2560d6e690d14df4ce77a3c3367a",
			executable: "769c7a60cbd0197dc140d71f2a4fb81a38853bb4302059966c1663d4913777f3",
		},
		105: {
			checksum:   "b64cde1b3bd74b2494cd85e660d2b56d2d60405378d92309af05085f030a0cc2",
			executable: "83154a4aee3a872c65687b8d425a103d1d9a6ec412c7151a8ac10512567ddb0e",
		},
		106: {
			checksum:   "1e20c3e3be4e149143800288704b43a2091773e8e51b07682c13a0c1bbd74577",
			executable: "aabce937db56112c92ad8b5fb91dcb9312f7abb3e2d870c15ba271f69346c2f5",
		},
		107: {
			checksum:   "64046a14261655b92d36001d299508647c83abdb22026bd46e1eccdb29d90c9e",
			executable: "ec983dc4079b04d54f25b21e5a5b485488d15572937e11d600065388b50e7a15",
		},
		108: {
			checksum:   "6f1a274b104e2171afe234258336a33b0f64beed3a1561196abb5978ff52b1fc",
			executable: "e80c010143b9336b97c22de2c6a42551134b4c847509bf114e8ad44a722dd045",
		},
		113: {
			checksum:   "730199e48efd32450e3be387fc78df0529e71a989893403aa74cdb06adc55166",
			executable: "eaae80a81fe1b7eebe21ebe0ba36ef7dcf0dd0e9b2f1b690218837da2db638bb",
		},
		114: {
			checksum:   "d176bb87333931a8c53985b93c75d9107bd30eba9f4477a2d79ff320d9a376f3",
			executable: "8ab08ccebac01a1b32e4e95047981ececd8e64085ddb2ab92100c423fcc3abad",
		},
		116: {
			checksum:   "f22ef765a9ec028c2523e5b1ca707c569a2913d7d69d7cfc8d47776be501cf48",
			executable: "febfff8b85d0fc4722a4d444caf71cd51dd1c06eb94a852c1be1a16fb6a6ab0a",
		},
		118: {
			checksum:   "ee30a0e5b26e1095a76fd98d22c15d330735d1cdd4bdb483447cc6312bc6a7aa",
			executable: "17eb7169bb6418fef5bc80ef30ab8d5a8593fa7b65ed64847332e6083cda39fd",
		},
		121: {
			checksum:   "525bdd030c70dc5090c8e1c5e51b167a15b0e94880a5d74a94981c16caaba54a",
			executable: "e35d48ce78f51beaf2634568d049dc4260b27ce1b66facf92ee6548c28e8b849",
		},
		122: {
			checksum:   "b738b2131327530879655b346a3ced6e5dcca6341d6f0e642f433403006cf938",
			executable: "037663e501ee0f583c4c1cd39cd805872e679eaefe497cc2927d9159952bbee1",
		},
		125: {
			checksum:   "5a975d1f9a33f6265c976ca6533ddfbfa858ac20026bd2c781f6d6f916291da0",
			executable: "a7ff5290221cb5d3b8f7214a6307747e57e76067aa3fd9da605efbe6a8c3194b",
		},
		134: {
			checksum:   "a64dbcba1c6080d36f1386952ea1fd5c21c9f38b4b0d453531a33f511fd82e76",
			executable: "6ddbbfefdc14dbc19b55fcff8871912527d54f7c07c88154d8ea17f2e908c3d6",
		},
		135: {
			checksum:   "a2e18d04fa6342f2c4ac2554db0d338d3d106ab057decd3ece3e9f56ddd86136",
			executable: "6e850f5aea076269442b6fd2b8e26d34b5d4cfdd5b96c23a22932459677e8151",
		},
		136: {
			checksum:   "43a088b132c930aee9932f50f0e8e90b159c1b6b65351471f8f7a2ad838db03a",
			executable: "a019a4a7f5b9cf9276975faebe44db2b50f89321057beabcff03795cbf9c9f12",
		},
		144: {
			checksum:   "57c75a9469905aeeb44bf65eb69dc18df4e567faeb2ec27eec93eaa427ae428a",
			executable: "df9d782496b57e31e9bb13cda94ec033593bac4a8eb3a581d697205c86f110d3",
		},
		156: {
			checksum:   "3046d0e8c33b278ae898edcb6be70512b3d49ef1825ef4e9bd009f929c9c378c",
			executable: "fbc44340772cc05bdab8bbcf450942c609aa2ffcd27aabfbffb45270fa7a686b",
		},
		158: {
			checksum:   "28a73d0b776a7d1f1eb478dad567c3501181c5977cea224872a4fdfd3cd0074b",
			executable: "85a7e3d7c067d5bd62d39f17aac67e3fa6cb133ff67a69fcae48d727410f313f",
		},
		159: {
			checksum:   "77fb59247302d490ed2ed36103197fb74d1d8f6a9f323ec164ba84a922f72a68",
			executable: "fc38b3264f66dc58eb51ec2e6593c4b9657fa483324acbcb0830364beedac505",
		},
	},
	ScopeTenant: {
		122: {
			checksum:   "262adb60df5e09f8eef194fcde6ae088a422753dadd434e2d377af3bc0e05f31",
			executable: "bf35f0040958fa0fde7e40cf737582c5a29128e59280e48281ed99a5a9de33fa",
		},
		123: {
			checksum:   "efe8bf7e8ef8e7dccd0e7aa97bcbfed4f3382a3b853583a620d79766236beff9",
			executable: "9cc2a1ee3b08ad40f23adf6ba69fb4bd5f96336191fdae366924e442654780ed",
		},
		124: {
			checksum:   "93a6073ba5e908b468544a52b117b039cf752e1bce496f462010e2c41b848f0e",
			executable: "8e7d67622eeca4785d13f00f05008fd31829e354531f91555dcc88a120e75b9e",
		},
		125: {
			checksum:   "411d3211fe4b8aca33afdb0b4b1bc65a1bd432f92741352ed92c69b1d1fb2403",
			executable: "51bf928a3f86bef711adc920583c2f3e9e9dc397a8c8bad086faab0f2e5c6bd8",
		},
		126: {
			checksum:   "f3ae2cf89dad14cda5d92634c8ebf109f077b5f7b4ee6c41516e4de043e4c02d",
			executable: "733260394fb207026831213fba1caf09f578cefa63e9b24e4faa1dc7be8712a8",
		},
		127: {
			checksum:   "e1eb775a10d8c6768897c4b22ed033c475869cc146fb687381bc754797afe953",
			executable: "f177ae63ef80d35199542d4fa0fc86ccc0b21d0af32fd9cc1ced36ea66acb307",
		},
		128: {
			checksum:   "167f2a194f5b09145e9827179220b3d1bbb0e1a1ec091be394f3b1f6b6df4c8d",
			executable: "9914ad7819d6f929613555c1044c63fe6573db412a36559eb65f83f1020c1582",
		},
		129: {
			checksum:   "2649d3430ffb74545d87237b851bdee38ce9b1764ca01eb15b7f0d735b086777",
			executable: "a790acb92b4fb18d2c6886cd03d20c5b5a43f763e007bd6059c1b2414c8a7b5d",
		},
		131: {
			checksum:   "cb2fd12586cd54f712d8350e4f97e2943d8e741d37c812a4edaae3cee82f9e96",
			executable: "01512370132d1fa91c109eebb5824331658fa157b3d887b8585107291943508e",
		},
		132: {
			checksum:   "2b2bc160917e8a6e416df48656dd81de7c4c273994884ce5abda26932286830f",
			executable: "e78280c323f8a7e14b954782417ccdb36b68923fe873bed1a98b9ac890696159",
		},
		134: {
			checksum:   "5c6c468a9c1a8a5d17550d7dcb406955b28d2b0ce3db186be4fb2f1af05c2f31",
			executable: "3b446ff5cc810c4740a13df7bbd48ce5dbaca1d1724bb86b63a09d0899b15619",
		},
		147: {
			checksum:   "13da81d23fed42dd497bdfa3011498825d09bde30de1b2fc2ce2c63a1301cc8b",
			executable: "70e50752f1cc85cbd7953866204f19de47df378747e8ba52d373e9abc5a01dec",
		},
		150: {
			checksum:   "f778b630483ae3c2e9949e1df722d207fd6073bcea5be9131474b5a887a8e3a5",
			executable: "8b668d70e4885bc29de590ad3cf33d3fbb81e60125b077126f31ee41ab654112",
		},
		151: {
			checksum:   "19e74029405aafb5768d68a857901df9c30880141dba25dd5ee587d8c81e7fc9",
			executable: "fda39ddd58f85d2ef12cd0f41e2158f4a9a9122497a63ffd701bbd52e758e017",
		},
	},
}

// releasedText reports whether recorded is the released checksum of a
// rewritten migration.
func releasedText(migration Migration, recorded []byte) bool {
	text, ok := releasedTexts[migration.Scope][migration.Version]
	if !ok {
		return false
	}
	want, err := hex.DecodeString(text.checksum)
	return err == nil && len(recorded) == len(want) && subtle.ConstantTimeCompare(recorded, want) == 1
}

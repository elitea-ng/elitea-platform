# Agents: default sub-agent avatars are a closed set (#1870, #1951)

- **What changed.** A `spawn_agent` `profile.avatar` must be one of `default:owl`, `default:hawk`,
  `default:fox`, `default:wolf`, `default:bear`, `default:otter`, `default:heron` or `default:lynx`.
  Before, any key of at most 128 bytes was accepted. Web, desktop and mobile now draw the key as an
  image. They draw the brand mark for a key they do not know.
- **Operator action.** None. An agent that names another avatar key gets a refusal that lists the
  allowed keys, and the model can retry. Stored keys from earlier runs still display: an unknown key
  shows the brand mark.

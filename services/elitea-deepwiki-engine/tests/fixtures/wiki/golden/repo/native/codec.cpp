#include "codec.h"

namespace notes {

std::string Codec::Encode(const std::string& body) const {
  return "v1:" + body;
}

std::string Codec::Decode(const std::string& data) const {
  return data.substr(3);
}

}  // namespace notes

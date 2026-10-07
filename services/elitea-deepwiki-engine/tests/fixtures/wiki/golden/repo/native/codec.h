#pragma once
#include <string>

namespace notes {

class Codec {
 public:
  std::string Encode(const std::string& body) const;
  std::string Decode(const std::string& data) const;
};

}  // namespace notes

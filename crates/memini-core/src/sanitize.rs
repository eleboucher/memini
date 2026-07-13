use unicode_script::{Script, UnicodeScript};

pub fn clean(value: &str) -> String {
    clean_bytes(value.as_bytes())
}
pub fn clean_bytes(value: &[u8]) -> String {
    String::from_utf8_lossy(value)
        .chars()
        .filter(|&c| {
            c != '\u{fffd}'
                && (c == '\t'
                    || c == '\n'
                    || c == '\r'
                    || (c >= '\u{20}' && !(('\u{7f}'..='\u{9f}').contains(&c))))
                && !is_non_character(c)
        })
        .collect()
}
fn is_non_character(c: char) -> bool {
    ('\u{fdd0}'..='\u{fdef}').contains(&c) || (c as u32 & 0xfffe) == 0xfffe
}

fn bucket(c: char) -> Option<u8> {
    if !c.is_alphabetic() {
        return None;
    }
    Some(match c.script() {
        Script::Han | Script::Hiragana | Script::Katakana | Script::Hangul | Script::Bopomofo => 1,
        Script::Latin => 2,
        Script::Cyrillic => 3,
        Script::Greek => 4,
        Script::Arabic => 5,
        Script::Hebrew => 6,
        _ => 0,
    })
}
pub fn garbled(value: &str) -> bool {
    let (mut letters, mut transitions, mut previous, mut adjacent) = (0, 0, 0, false);
    for c in value.chars() {
        match bucket(c) {
            None => adjacent = false,
            Some(current) => {
                letters += 1;
                if adjacent && previous != 0 && current != 0 && previous != current {
                    transitions += 1;
                }
                previous = current;
                adjacent = true;
            }
        }
    }
    letters >= 12 && transitions >= 6 && transitions as f64 / letters as f64 >= 0.20
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn clean_contract() {
        assert_eq!(clean("a\0\u{7f}b\u{fffd}c\u{fdd0}d"), "abcd");
        assert_eq!(clean("使用React框架 🚀\n"), "使用React框架 🚀\n");
        assert_eq!(clean_bytes(b"ab\xffcd"), "abcd");
    }
    #[test]
    fn garble_contract() {
        assert!(garbled(
            "Thank you I'm这a家b制c品d with在e上f世g纪h and的i more"
        ));
        assert!(!garbled("使用React框架开发应用程序非常方便快捷"));
        assert!(!garbled("the 这个 thing is separated"));
    }
}

// Looking a species up in BirdNET's list by either name, as the Review rules
// page's "Add a species" does.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { fold, speciesIndex } from "../../frontend/js/species-lookup.js";

const index = speciesIndex([
  ["Strix varia", "Barred Owl"],
  ["Glaucidium gnoma", "Northern Pygmy-Owl"],
  ["Poecile atricapillus", "Black-capped Chickadee"],
  ["Catharus ustulatus", "Swainson's Thrush"],
  ["Corthylio calendula", "Ruby-crowned Kinglet"],
  ["Bubo virginianus", "Great Horned Owl"],
  ["Dog", "Dog"],
]);

test("either name finds the species, as BirdNET spells it", () => {
  const owl = { scientificName: "Strix varia", commonName: "Barred Owl" };
  assert.deepEqual(index.find("Strix varia"), owl);
  assert.deepEqual(index.find("  barred   OWL "), owl);
  assert.deepEqual(index.find("strix varia", "scientific"), owl);
  assert.equal(index.find("Barred Owl", "scientific"), null);
  assert.deepEqual(index.find("Barred Owl", "common"), owl);
  assert.equal(index.find("Regulus calendula"), null);
  assert.equal(index.find(""), null);
});

test("a hyphen, a space and a curly apostrophe are one another", () => {
  assert.equal(fold("Black-capped  Chickadee"), "black capped chickadee");
  assert.equal(index.find("black capped chickadee")?.scientificName, "Poecile atricapillus");
  assert.equal(index.find("Northern Pygmy Owl")?.scientificName, "Glaucidium gnoma");
  assert.equal(index.find("Swainson’s Thrush")?.scientificName, "Catharus ustulatus");
});

test("suggestions begin with the name, then a word of it, then anything containing it", () => {
  const names = (text, kind) => index.suggest(text, kind).map((s) => s.commonName);
  assert.deepEqual(names("owl", "common"), ["Barred Owl", "Great Horned Owl", "Northern Pygmy-Owl"]);
  assert.deepEqual(names("gr", "common"), ["Great Horned Owl"]);
  assert.deepEqual(names("o", "common"), [], "one letter suggests nothing");
  assert.deepEqual(
    index.suggest("str", "scientific").map((s) => s.scientificName),
    ["Strix varia"],
  );
  assert.deepEqual(
    index.suggest("ar", "scientific").map((s) => s.scientificName),
    ["Catharus ustulatus", "Strix varia"],
    "neither begins with it, so by name",
  );
  assert.deepEqual(
    index.suggest("us", "scientific").map((s) => s.scientificName),
    ["Catharus ustulatus", "Bubo virginianus", "Poecile atricapillus"],
    "a word beginning with it comes before a name merely containing it",
  );
  assert.equal(index.suggest("e", "common", 2).length, 0);
  assert.equal(index.suggest("ow", "common", 2).length, 2, "at most the limit");
});

test("the shipped list is BirdNET's, one name to one species", () => {
  const list = JSON.parse(readFileSync(new URL("../../frontend/data/birdnet-species.json", import.meta.url)));
  const all = speciesIndex(list.species);
  assert.equal(all.size, list.species.length);
  assert.ok(all.size > 6000);
  assert.equal(new Set(list.species.map(([s]) => fold(s))).size, all.size, "no two scientific names fold alike");
  assert.equal(new Set(list.species.map(([, c]) => fold(c))).size, all.size, "no two common names fold alike");
  assert.equal(all.find("Barred Owl")?.scientificName, "Strix varia");
  assert.equal(all.find("Ruby-crowned Kinglet")?.scientificName, "Corthylio calendula");
});
